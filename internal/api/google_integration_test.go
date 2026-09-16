package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type googleFlow struct {
	state, verifier, code string
	cookie                *http.Cookie
	params                url.Values
}

func setupGoogle(t *testing.T) (*frontHarness, *fakeGoogle) {
	t.Helper()
	f := newFrontendHarness(t)
	g := newFakeGoogle(t)
	c := googleTestConfig()
	f.s.C.GoogleEnabled = true
	f.s.C.GoogleClientID = c.GoogleClientID
	f.s.C.GoogleClientSecret = c.GoogleClientSecret
	f.s.C.GoogleRedirectURL = c.GoogleRedirectURL
	f.s.C.GoogleReturnURLs = c.GoogleReturnURLs
	f.s.google = newGoogleOAuth(c, g.client)
	return f, g
}

func googleRequest(f *frontHarness, method, path string, b any, cookie *http.Cookie) *httptest.ResponseRecorder {
	var payload []byte
	if b != nil {
		payload, _ = json.Marshal(b)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(payload))
	r.RemoteAddr = "127.0.0.1:1234"
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func beginGoogle(t *testing.T, f *frontHarness, g *fakeGoogle) googleFlow {
	t.Helper()
	v := randomToken()
	start := object(t, f.call("POST", "/v1/auth/google/start", "", map[string]string{"code_challenge": challenge(v), "redirect_uri": "broto://auth-callback"}, 200))
	state := start["state"].(string)
	w := googleRequest(f, "GET", start["url"].(string), nil, nil)
	if w.Code != 302 {
		t.Fatalf("authorize status %d: %s", w.Code, w.Body)
	}
	u, e := url.Parse(w.Header().Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	p := u.Query()
	if u.Host != "accounts.google.com" || p.Get("state") != state || p.Get("code_challenge_method") != "S256" || p.Get("nonce") == "" || p.Get("scope") != "openid email profile" || p.Get("client_secret") != "" || p.Get("redirect_uri") != googleTestConfig().GoogleRedirectURL {
		t.Fatal("incorrect Google authorization parameters")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].MaxAge != 600 {
		t.Fatal("unsafe browser cookie")
	}
	return googleFlow{state: state, verifier: v, code: g.grant(p), cookie: cookies[0], params: p}
}

func callbackPath(flow googleFlow) string {
	return "/v1/auth/google/callback?" + url.Values{"state": {flow.state}, "code": {flow.code}}.Encode()
}

func finishGoogle(t *testing.T, f *frontHarness, flow googleFlow, wantError string) string {
	t.Helper()
	w := googleRequest(f, "GET", callbackPath(flow), nil, flow.cookie)
	if w.Code != 302 {
		t.Fatalf("callback status %d: %s", w.Code, w.Body)
	}
	u, e := url.Parse(w.Header().Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	if u.Scheme != "broto" || u.Host != "auth-callback" || q.Get("state") != flow.state || q.Get("error") != wantError || q.Get("access_token") != "" {
		t.Fatal("incorrect app return", u)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("callback leaked cache/referrer")
	}
	if wantError == "" && len(q.Get("code")) != 64 {
		t.Fatal("missing app code")
	}
	if wantError != "" && q.Get("code") != "" {
		t.Fatal("failure returned a code")
	}
	return q.Get("code")
}

func exchangeGoogle(t *testing.T, f *frontHarness, flow googleFlow, code string) map[string]any {
	t.Helper()
	return object(t, f.call("POST", "/v1/auth/google/exchange", "", map[string]string{"code": code, "code_verifier": flow.verifier}, 200))
}

func TestGoogleLoginSessionAndDeletion(t *testing.T) {
	f, g := setupGoogle(t)
	for _, b := range []map[string]string{
		{"redirect_uri": "https://evil.invalid", "code_challenge": challenge(randomToken())},
		{"redirect_uri": "broto://auth-callback?next=evil", "code_challenge": challenge(randomToken())},
		{"redirect_uri": "broto://auth-callback", "code_challenge": "plain"},
	} {
		f.call("POST", "/v1/auth/google/start", "", b, 400)
	}
	flow := beginGoogle(t, f, g)
	var stateHash, nonceHash, browserHash string
	if e := f.s.DB.QueryRow(context.Background(), "select state_hash,nonce_hash,browser_hash from oauth_attempts").Scan(&stateHash, &nonceHash, &browserHash); e != nil || stateHash != hash(flow.state) || nonceHash != hash(flow.params.Get("nonce")) || browserHash != hash(flow.cookie.Value) {
		t.Fatal("attempt hashing incorrect", e)
	}
	code := finishGoogle(t, f, flow, "")
	var sessions int
	if e := f.s.DB.QueryRow(context.Background(), "select count(*) from sessions").Scan(&sessions); e != nil || sessions != 0 {
		t.Fatal("callback issued a session before PKCE exchange", e)
	}
	w := googleRequest(f, "GET", callbackPath(flow), nil, flow.cookie)
	if w.Code != 400 {
		t.Fatal("callback replay accepted")
	}
	f.call("POST", "/v1/auth/google/exchange", "", map[string]string{"code": code, "code_verifier": randomToken()}, 400)
	session := exchangeGoogle(t, f, flow, code)
	token := session["access_token"].(string)
	u := session["user"].(map[string]any)
	id := u["id"].(string)
	if u["email"] != "garden@example.invalid" || u["email_confirmed_at"] == nil || u["user_metadata"].(map[string]any)["name"] != "Maria" {
		t.Fatal("session contract mismatch")
	}
	f.call("POST", "/v1/auth/google/exchange", "", map[string]string{"code": code, "code_verifier": flow.verifier}, 400)
	f.call("GET", "/v1/auth/user", token, nil, 200)
	profile := object(t, f.call("GET", "/v1/data/profiles", token, nil, 200))
	if profile["id"] != id || profile["accepted_terms_at"] != nil {
		t.Fatal("profile or consent was not preserved")
	}
	var password string
	if e := f.s.DB.QueryRow(context.Background(), "select password_hash from users where id=$1", id).Scan(&password); e != nil || password != "!google-only" {
		t.Fatal("unexpected Google password", e)
	}
	// Same stable subject, even with changed Google email, must keep the account.
	g.claims["email"] = "changed@example.invalid"
	again := beginGoogle(t, f, g)
	second := exchangeGoogle(t, f, again, finishGoogle(t, f, again, ""))
	if second["user"].(map[string]any)["id"] != id {
		t.Fatal("Google login duplicated account")
	}
	f.call("DELETE", "/v1/account", token, nil, 200)
	f.call("GET", "/v1/auth/user", second["access_token"].(string), nil, 401)
	var identities int
	if e := f.s.DB.QueryRow(context.Background(), "select count(*) from oauth_identities").Scan(&identities); e != nil || identities != 0 {
		t.Fatal("identity was not deleted", e)
	}
}

func TestGoogleExistingAccountRequiresIdentityMigration(t *testing.T) {
	f, g := setupGoogle(t)
	id, token := f.account()
	plant := f.plant(token, nil)
	f.exec("update users set email='garden@example.invalid' where id=$1", id)
	flow := beginGoogle(t, f, g)
	finishGoogle(t, f, flow, "google_email_em_uso")
	var identities int
	if e := f.s.DB.QueryRow(context.Background(), "select count(*) from oauth_identities").Scan(&identities); e != nil || identities != 0 {
		t.Fatal("silently linked existing email", e)
	}
	// This represents an explicit, trusted import of the original Google subject.
	f.exec("insert into oauth_identities(provider,subject,user_id) values('google','google-subject-123',$1)", id)
	flow = beginGoogle(t, f, g)
	session := exchangeGoogle(t, f, flow, finishGoogle(t, f, flow, ""))
	if session["user"].(map[string]any)["id"] != id {
		t.Fatal("migrated identity lost original account")
	}
	f.call("GET", "/v1/data/plants/"+plant, session["access_token"].(string), nil, 200)
}

func TestGoogleCallbackBrowserStateAndCancellation(t *testing.T) {
	f, g := setupGoogle(t)
	flow := beginGoogle(t, f, g)
	for _, cookie := range []*http.Cookie{nil, {Name: flow.cookie.Name, Value: randomToken()}} {
		w := googleRequest(f, "GET", callbackPath(flow), nil, cookie)
		if w.Code != 400 {
			t.Fatal("callback accepted foreign browser")
		}
	}
	for _, path := range []string{
		"/v1/auth/google/callback?state=" + randomToken() + "&code=test",
		callbackPath(flow) + "&state=" + flow.state,
		"/v1/auth/google/authorize?state=" + flow.state,
	} {
		w := googleRequest(f, "GET", path, nil, flow.cookie)
		if w.Code != 400 {
			t.Fatal("invalid/replayed state accepted")
		}
	}
	if g.tokens != 0 {
		t.Fatal("contacted provider for invalid callback")
	}
	w := googleRequest(f, "GET", "/v1/auth/google/callback?"+url.Values{"state": {flow.state}, "error": {"access_denied"}}.Encode(), nil, flow.cookie)
	if w.Code != 302 || !strings.Contains(w.Header().Get("Location"), "error=google_login_cancelado") {
		t.Fatal("cancellation was not reported")
	}
	if g.tokens != 0 {
		t.Fatal("contacted provider after cancellation")
	}
	w = googleRequest(f, "GET", callbackPath(flow), nil, flow.cookie)
	if w.Code != 400 {
		t.Fatal("cancelled attempt replayed")
	}
	expired := beginGoogle(t, f, g)
	f.exec("update oauth_attempts set expires_at=now()-interval '1 second'")
	w = googleRequest(f, "GET", callbackPath(expired), nil, expired.cookie)
	if w.Code != 400 || g.tokens != 0 {
		t.Fatal("expired attempt accepted")
	}
}

func TestGoogleProviderFailureCreatesNoAccount(t *testing.T) {
	f, g := setupGoogle(t)
	for _, failure := range []string{"token", "network", "signature", "missing token"} {
		g.failure = failure
		flow := beginGoogle(t, f, g)
		finishGoogle(t, f, flow, "google_autenticacao_falhou")
	}
	var users, codes, sessions int
	if e := f.s.DB.QueryRow(context.Background(), "select (select count(*) from users),(select count(*) from oauth_codes),(select count(*) from sessions)").Scan(&users, &codes, &sessions); e != nil || users+codes+sessions != 0 {
		t.Fatal("provider failure left authentication records", users, codes, sessions, e)
	}
}

func TestGoogleConcurrentCallbackAndIdentity(t *testing.T) {
	f, g := setupGoogle(t)
	flow := beginGoogle(t, f, g)
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 8)
	for i := 0; i < 8; i++ {
		go func() { <-start; results <- googleRequest(f, "GET", callbackPath(flow), nil, flow.cookie) }()
	}
	close(start)
	var codes []string
	for i := 0; i < 8; i++ {
		w := <-results
		if w.Code == 302 {
			u, _ := url.Parse(w.Header().Get("Location"))
			codes = append(codes, u.Query().Get("code"))
		} else if w.Code != 400 {
			t.Errorf("unexpected callback status %d", w.Code)
		}
	}
	if len(codes) != 1 || codes[0] == "" || g.tokens != 1 {
		t.Fatal("callback was not single use", len(codes), g.tokens)
	}
	first := exchangeGoogle(t, f, flow, codes[0])
	id := first["user"].(map[string]any)["id"]
	// Independent simultaneous logins for the same Google subject must share a user.
	flows := []googleFlow{beginGoogle(t, f, g), beginGoogle(t, f, g)}
	type result struct {
		flow googleFlow
		w    *httptest.ResponseRecorder
	}
	pair := make(chan result, 2)
	for _, flow := range flows {
		go func(flow googleFlow) {
			pair <- result{flow, googleRequest(f, "GET", callbackPath(flow), nil, flow.cookie)}
		}(flow)
	}
	for i := 0; i < 2; i++ {
		r := <-pair
		if r.w.Code != 302 {
			t.Errorf("concurrent identity callback failed: %d", r.w.Code)
			continue
		}
		u, _ := url.Parse(r.w.Header().Get("Location"))
		code := u.Query().Get("code")
		if code == "" {
			t.Errorf("concurrent identity failed: %s", u.Query().Get("error"))
			continue
		}
		if session := exchangeGoogle(t, f, r.flow, code); session["user"].(map[string]any)["id"] != id {
			t.Error("duplicate Google user")
		}
	}
	var users, identities int
	if e := f.s.DB.QueryRow(context.Background(), "select (select count(*) from users),(select count(*) from oauth_identities)").Scan(&users, &identities); e != nil || users != 1 || identities != 1 {
		t.Fatal("concurrent login duplicated account", users, identities, e)
	}
}

func TestGoogleExchangeExpiryConcurrencyAndRollback(t *testing.T) {
	t.Run("expiry and concurrent exchange", func(t *testing.T) {
		f, g := setupGoogle(t)
		flow := beginGoogle(t, f, g)
		code := finishGoogle(t, f, flow, "")
		f.exec("update oauth_codes set expires_at=now()-interval '1 second'")
		f.call("POST", "/v1/auth/google/exchange", "", map[string]string{"code": code, "code_verifier": flow.verifier}, 400)
		flow = beginGoogle(t, f, g)
		code = finishGoogle(t, f, flow, "")
		start := make(chan struct{})
		results := make(chan *httptest.ResponseRecorder, 8)
		for i := 0; i < 8; i++ {
			go func() {
				<-start
				results <- googleRequest(f, "POST", "/v1/auth/google/exchange", map[string]string{"code": code, "code_verifier": flow.verifier}, nil)
			}()
		}
		close(start)
		winners := 0
		for i := 0; i < 8; i++ {
			w := <-results
			if w.Code == 200 {
				winners++
			} else if w.Code != 400 {
				t.Errorf("unexpected exchange status %d", w.Code)
			}
		}
		var count int
		if e := f.s.DB.QueryRow(context.Background(), "select count(*) from sessions").Scan(&count); e != nil || count != 1 || winners != 1 {
			t.Fatal("exchange was not atomic", count, winners, e)
		}
	})
	t.Run("session commit failure preserves code for retry", func(t *testing.T) {
		f, g := setupGoogle(t)
		flow := beginGoogle(t, f, g)
		code := finishGoogle(t, f, flow, "")
		f.exec(`create function fail_oauth_commit() returns trigger language plpgsql as $$ begin raise exception 'injected session commit failure'; end $$;
		create constraint trigger fail_oauth_commit after insert on sessions deferrable initially deferred for each row execute function fail_oauth_commit()`)
		f.call("POST", "/v1/auth/google/exchange", "", map[string]string{"code": code, "code_verifier": flow.verifier}, 500)
		var pending, sessions int
		if e := f.s.DB.QueryRow(context.Background(), "select (select count(*) from oauth_codes where code_hash=$1),(select count(*) from sessions)", hash(code)).Scan(&pending, &sessions); e != nil || pending != 1 || sessions != 0 {
			t.Fatal("commit failure did not roll back", pending, sessions, e)
		}
		f.exec("drop trigger fail_oauth_commit on sessions")
		exchangeGoogle(t, f, flow, code)
	})
}

func TestGoogleExpiredRecordsCleanup(t *testing.T) {
	f := newFrontendHarness(t)
	id, _ := f.account()
	for _, expiry := range []string{"now()-interval '1 second'", "now()+interval '1 hour'"} {
		f.exec("insert into oauth_attempts(state_hash,app_challenge,return_uri,expires_at) values($1,$2,'broto://auth-callback',"+expiry+")", hash(randomToken()), challenge(randomToken()))
		f.exec("insert into oauth_codes(code_hash,user_id,app_challenge,expires_at) values($1,$2,$3,"+expiry+")", hash(randomToken()), id, challenge(randomToken()))
	}
	ctx := context.Background()
	tx, e := f.s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer rollback(tx)
	if e = f.s.cleanup(ctx, tx); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	var attempts, codes int
	if e = f.s.DB.QueryRow(ctx, "select (select count(*) from oauth_attempts),(select count(*) from oauth_codes)").Scan(&attempts, &codes); e != nil || attempts != 1 || codes != 1 {
		t.Fatal("incorrect OAuth expiry cleanup", attempts, codes, e)
	}
}
