package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type frontHarness struct {
	t *testing.T
	s *Server
	h http.Handler
}

func newFrontendHarness(t *testing.T) *frontHarness {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required; run sh scripts/test-integration.sh")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { admin.Close(ctx) })
	name := "broto_front_" + randomToken()[:12]
	if _, e = admin.Exec(ctx, "create database "+name); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if _, e = admin.Exec(ctx, "drop database "+name+" with (force)"); e != nil {
			t.Error(e)
		}
	})
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	u.Path = "/" + name
	c := Config{DatabaseURL: u.String(), StorageDir: t.TempDir(), SigningKey: strings.Repeat("k", 32), DevAuth: true, StorageQuota: 1 << 20, PublicURL: "http://localhost:8080", Origins: "http://localhost:8081", DeepSeekKey: "test-only", VisionModel: "vision-test", FactsModel: "facts-test", ChatModel: "chat-test"}
	s, e := New(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.DB.Close)
	s.HTTP = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unmocked external request blocked by test")
	})}
	return &frontHarness{t, s, s.Routes()}
}
func (f *frontHarness) call(method, path, token string, b any, want int) json.RawMessage {
	f.t.Helper()
	var reader io.Reader
	if raw, ok := b.([]byte); ok {
		reader = bytes.NewReader(raw)
	} else if b != nil {
		data, e := json.Marshal(b)
		if e != nil {
			f.t.Fatal(e)
		}
		reader = bytes.NewReader(data)
	}
	r := httptest.NewRequest(method, path, reader)
	r.RemoteAddr = "127.0.0.1:1234"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != want {
		f.t.Fatalf("%s %s: got %d want %d: %.800s", method, path, w.Code, want, w.Body.String())
	}
	return w.Body.Bytes()
}
func (f *frontHarness) exec(q string, args ...any) {
	f.t.Helper()
	if _, e := f.s.DB.Exec(context.Background(), q, args...); e != nil {
		f.t.Fatal(e)
	}
}
func (f *frontHarness) account() (id, token string) {
	f.t.Helper()
	token = randomToken()
	// Most tests do not exercise bcrypt; seed accounts instead of weakening auth
	// configuration or spending CPU hashing a password for every independent case.
	if e := f.s.DB.QueryRow(context.Background(), "insert into users(email,password_hash,email_confirmed_at,raw_user_meta_data) values($1,'not-a-password',now(),'{\"name\":\"Jardineira\"}') returning id::text", randomToken()[:12]+"@example.invalid").Scan(&id); e != nil {
		f.t.Fatal(e)
	}
	f.exec("insert into sessions(token_hash,user_id,expires_at) values($1,$2,now()+interval '30 days')", hash(token), id)
	return
}
func object(t *testing.T, b json.RawMessage) map[string]any {
	t.Helper()
	var v map[string]any
	if e := json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func records(t *testing.T, b json.RawMessage) []map[string]any {
	t.Helper()
	var v []map[string]any
	if e := json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	if v == nil {
		t.Fatal("expected JSON array, got null")
	}
	return v
}
func (f *frontHarness) plant(token string, extra map[string]any) string {
	f.t.Helper()
	b := map[string]any{"nickname": "Jiboia"}
	for k, v := range extra {
		b[k] = v
	}
	return object(f.t, f.call("POST", "/v1/data/plants", token, b, 201))["id"].(string)
}
func (f *frontHarness) photo(token string) string {
	f.t.Helper()
	return object(f.t, f.call("POST", "/v1/photos", token, append([]byte{0xff, 0xd8, 0xff}, make([]byte, 128)...), 201))["path"].(string)
}
