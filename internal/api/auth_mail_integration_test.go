package api

import (
	"context"
	"io"
	"net/mail"
	"net/url"
	"strings"
	"testing"
)

func configureLocalSMTP(f *frontHarness, m *localSMTP) {
	c := m.config()
	f.s.C.SMTPHost = c.SMTPHost
	f.s.C.SMTPPort = c.SMTPPort
	f.s.C.SMTPUser = c.SMTPUser
	f.s.C.SMTPPassword = c.SMTPPassword
	f.s.C.SMTPFrom = c.SMTPFrom
	f.s.C.SiteURL = c.SiteURL
	f.s.C.DevAuth = false
	f.s.smtpRoots = m.roots
}
func lastMailLink(t *testing.T, m *localSMTP) *url.URL {
	t.Helper()
	messages, _, _ := m.snapshot()
	if len(messages) == 0 {
		t.Fatal("no captured mail")
	}
	message, e := mail.ReadMessage(strings.NewReader(messages[len(messages)-1]))
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(message.Body)
	if e != nil {
		t.Fatal(e)
	}
	for _, field := range strings.Fields(string(b)) {
		if strings.HasPrefix(field, "https://app.example.invalid/") {
			u, e := url.Parse(field)
			if e != nil {
				t.Fatal(e)
			}
			return u
		}
	}
	t.Fatal("mail does not contain the expected site link")
	return nil
}
func TestAuthSMTPConfirmationAndRecovery(t *testing.T) {
	f := newFrontendHarness(t)
	m := newLocalSMTP(t, "")
	configureLocalSMTP(f, m)
	b := map[string]string{"email": "smtp-flow@example.invalid", "password": "Senha#2026", "name": "Maria"}
	f.call("POST", "/v1/auth/signup", "", b, 202)
	f.call("POST", "/v1/auth/login", "", map[string]string{"email": b["email"], "password": b["password"]}, 403)
	first := lastMailLink(t, m)
	if first.Path != "/confirmado" || first.Query().Get("type") != "email" {
		t.Fatal("incorrect confirmation URL")
	}
	var stored, kind string
	var ttl bool
	if e := f.s.DB.QueryRow(context.Background(), "select token_hash,kind,expires_at>now() and expires_at<now()+interval '61 minutes' from auth_tokens where token_hash=$1", hash(first.Query().Get("token_hash"))).Scan(&stored, &kind, &ttl); e != nil || !ttl || kind != "email" || stored == first.Query().Get("token_hash") {
		t.Fatal("token storage or expiry incorrect", e)
	}
	f.call("POST", "/v1/auth/resend", "", map[string]string{"email": b["email"]}, 200)
	resent := lastMailLink(t, m)
	if resent.Query().Get("token_hash") == first.Query().Get("token_hash") {
		t.Fatal("resend reused token")
	}
	verify := func(u *url.URL, want int) map[string]any {
		return object(t, f.call("POST", "/v1/auth/verify", "", map[string]string{"token_hash": u.Query().Get("token_hash"), "type": u.Query().Get("type")}, want))
	}
	session := verify(resent, 200)
	old := session["access_token"].(string)
	verify(resent, 400)
	f.call("POST", "/v1/auth/recover", "", map[string]string{"email": b["email"]}, 200)
	recovery := lastMailLink(t, m)
	if recovery.Path != "/nova-senha" || recovery.Query().Get("type") != "recovery" {
		t.Fatal("incorrect recovery URL")
	}
	recovered := verify(recovery, 200)["access_token"].(string)
	f.call("GET", "/v1/auth/user", old, nil, 401)
	updated := object(t, f.call("PATCH", "/v1/auth/password", recovered, map[string]string{"password": "NovaSenha#2026"}, 200))
	f.call("GET", "/v1/auth/user", recovered, nil, 401)
	f.call("GET", "/v1/auth/user", updated["access_token"].(string), nil, 200)
	f.call("POST", "/v1/auth/login", "", map[string]string{"email": b["email"], "password": b["password"]}, 401)
	f.call("POST", "/v1/auth/login", "", map[string]string{"email": b["email"], "password": "NovaSenha#2026"}, 200)
	f.call("POST", "/v1/auth/recover", "", map[string]string{"email": b["email"]}, 200)
	expired := lastMailLink(t, m)
	f.exec("update auth_tokens set expires_at=now()-interval '1 second' where token_hash=$1", hash(expired.Query().Get("token_hash")))
	verify(expired, 400)
	before, _, _ := m.snapshot()
	f.call("POST", "/v1/auth/recover", "", map[string]string{"email": "unknown@example.invalid"}, 200)
	after, _, _ := m.snapshot()
	if len(after) != len(before) {
		t.Fatal("sent mail to nonexistent account")
	}
}
func TestAuthSMTPFailureCanRetry(t *testing.T) {
	f := newFrontendHarness(t)
	broken := newLocalSMTP(t, "RCPT")
	configureLocalSMTP(f, broken)
	b := map[string]string{"email": "retry@example.invalid", "password": "Senha#2026"}
	failed := object(t, f.call("POST", "/v1/auth/signup", "", b, 503))
	if failed["erro"] != "email_indisponivel_use_recover" {
		t.Fatal(failed)
	}
	var users, sessions int
	if e := f.s.DB.QueryRow(context.Background(), "select count(*) from users where email=$1", b["email"]).Scan(&users); e != nil || users != 1 {
		t.Fatal("registration must remain recoverable", e)
	}
	if e := f.s.DB.QueryRow(context.Background(), "select count(*) from sessions").Scan(&sessions); e != nil || sessions != 0 {
		t.Fatal("unconfirmed user has a session", e)
	}
	working := newLocalSMTP(t, "")
	configureLocalSMTP(f, working)
	f.call("POST", "/v1/auth/resend", "", map[string]string{"email": b["email"]}, 200)
	link := lastMailLink(t, working)
	f.call("POST", "/v1/auth/verify", "", map[string]string{"token_hash": link.Query().Get("token_hash"), "type": "email"}, 200)
}
