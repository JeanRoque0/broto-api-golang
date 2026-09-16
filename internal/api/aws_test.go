package api

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testCDN(t *testing.T) *photoCDN {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	c, e := newPhotoCDN("https://photos.example.com", "K123", string(pemKey))
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestCloudFrontSignature(t *testing.T) {
	c := testCDN(t)
	expires := time.Unix(1800000000, 0)
	signed, e := c.signedURL("owner/image.jpg", expires)
	if e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse(signed)
	raw := strings.NewReplacer("-", "+", "_", "=", "~", "/").Replace(u.Query().Get("Signature"))
	signature, e := base64.StdEncoding.DecodeString(raw)
	if e != nil {
		t.Fatal(e)
	}
	policy := `{"Statement":[{"Resource":"https://photos.example.com/photos/owner/image.jpg","Condition":{"DateLessThan":{"AWS:EpochTime":1800000000}}}]}`
	digest := sha1.Sum([]byte(policy))
	if e = rsa.VerifyPKCS1v15(&c.key.PublicKey, crypto.SHA1, digest[:], signature); e != nil {
		t.Fatal(e)
	}
	tampered := sha1.Sum([]byte(strings.ReplaceAll(policy, "image.jpg", "other.jpg")))
	if rsa.VerifyPKCS1v15(&c.key.PublicKey, crypto.SHA1, tampered[:], signature) == nil {
		t.Fatal("tampered resource accepted")
	}
	if u.Query().Get("Key-Pair-Id") != "K123" || u.Query().Get("Expires") != "1800000000" {
		t.Fatal("missing signed URL metadata")
	}
	for _, base := range []string{"http://example.com", "https://example.com/?x=1", "https://user:pass@example.com", ""} {
		if _, e := newPhotoCDN(base, "K123", "invalid-secret"); e == nil || strings.Contains(e.Error(), "invalid-secret") {
			t.Fatal("invalid config accepted or leaked")
		}
	}
}
func TestProxyTrustBoundary(t *testing.T) {
	prefixes, e := parseTrustedProxies("10.42.0.0/24")
	if e != nil {
		t.Fatal(e)
	}
	s := &Server{trustedProxies: prefixes}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "198.51.100.10:1234"
	r.Header.Set("X-Forwarded-For", "192.0.2.99")
	if s.clientIP(r) != "198.51.100.10" {
		t.Fatal("untrusted client spoofed IP")
	}
	r.RemoteAddr = "10.42.0.10:1234"
	r.Header.Set("X-Forwarded-For", "192.0.2.99, 198.51.100.10")
	if s.clientIP(r) != "198.51.100.10" {
		t.Fatal("did not use last untrusted hop")
	}
	if _, e := parseTrustedProxies("0.0.0.0/0"); e == nil {
		t.Fatal("unsafe proxy range accepted")
	}
}
func TestDatabaseEnvironmentEscapesPassword(t *testing.T) {
	for k, v := range map[string]string{"DATABASE_URL": "", "DB_HOST": "database.internal", "DB_PORT": "5432", "DB_NAME": "broto", "DB_USER": "broto_app", "DB_PASSWORD": "quote'@?#:/\\value", "DB_SSLMODE": "verify-full", "DB_SSLROOTCERT": "/ca.pem"} {
		t.Setenv(k, v)
	}
	dsn, e := databaseURL()
	if e != nil {
		t.Fatal(e)
	}
	c, e := pgx.ParseConfig(dsn)
	if e != nil { // ParseConfig opens the root cert; inspect URL directly instead.
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		pw, _ := u.User.Password()
		if pw != "quote'@?#:/\\value" {
			t.Fatal("password corrupted")
		}
		return
	}
	if c.Password != "quote'@?#:/\\value" {
		t.Fatal("password corrupted")
	}
}
func TestCDNOwnershipAndBackpressure(t *testing.T) {
	f := newFrontendHarness(t)
	_, token := f.account()
	_, other := f.account()
	path := f.photo(token)
	f.s.cdn = testCDN(t)
	f.call("POST", "/v1/photos/sign", other, map[string]string{"path": path}, 403)
	result := object(t, f.call("POST", "/v1/photos/sign", token, map[string]string{"path": path}, 200))
	if result["expiresIn"] != float64(300) || !strings.HasPrefix(result["signedUrl"].(string), "https://photos.example.com/photos/") {
		t.Fatal("CDN contract mismatch")
	}
	for i := 0; i < cap(f.s.requestSlots); i++ {
		f.s.requestSlots <- struct{}{}
	}
	f.call("GET", "/v1/auth/user", token, nil, 503)
	f.call("GET", "/livez", "", nil, 200)
	for i := 0; i < cap(f.s.requestSlots); i++ {
		<-f.s.requestSlots
	}
}
func TestRuntimeRoleCannotChangeSchema(t *testing.T) {
	f := newFrontendHarness(t)
	ctx := context.Background()
	password := strings.Repeat("a", 32) + "'\\quote"
	if e := provisionRuntimeRole(ctx, f.s.DB, password); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(f.s.C.DatabaseURL)
	if e != nil {
		t.Fatal(e)
	}
	u.User = url.UserPassword("broto_app", password)
	db, e := pgx.Connect(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close(ctx)
	if _, e = db.Exec(ctx, "create table public.forbidden(id int)"); e == nil {
		t.Fatal("runtime can create tables")
	}
	if _, e = db.Exec(ctx, "alter table public.users add column forbidden text"); e == nil {
		t.Fatal("runtime owns schema")
	}
	if _, e = db.Exec(ctx, "insert into users(email,password_hash) values($1,'hash')", fmt.Sprintf("runtime-%s@example.invalid", randomToken())); e != nil {
		t.Fatal(e)
	}
	c := f.s.C
	c.DatabaseURL = u.String()
	c.DisableMigrations = true
	runtime, e := New(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	runtime.DB.Close()
	f.exec("delete from schema_migrations where name=(select max(name) from schema_migrations)")
	if _, e = New(ctx, c); e == nil {
		t.Fatal("pending migration accepted")
	}
}
