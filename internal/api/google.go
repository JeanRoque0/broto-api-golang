package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

const googleIssuer = "https://accounts.google.com"
const googleKeysURL = "https://www.googleapis.com/oauth2/v3/certs"

var pkceVerifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

type googleOAuth struct {
	config   oauth2.Config
	verifier *oidc.IDTokenVerifier
	http     *http.Client
}

func validateGoogleConfig(c Config) error {
	if !c.GoogleEnabled {
		return nil
	}
	if !strings.HasSuffix(c.GoogleClientID, ".apps.googleusercontent.com") || strings.ContainsAny(c.GoogleClientID, ", \r\n\t") || strings.TrimSpace(c.GoogleClientSecret) == "" {
		return errors.New("Google OAuth requires a single web GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET")
	}
	u, e := url.Parse(c.GoogleRedirectURL)
	if e != nil || !safeOAuthURL(u, false) || u.Path != "/v1/auth/google/callback" {
		return errors.New("GOOGLE_REDIRECT_URL must be an HTTPS callback URL (HTTP loopback allowed locally)")
	}
	if strings.TrimSpace(c.GoogleReturnURLs) == "" {
		return errors.New("GOOGLE_RETURN_URLS is required")
	}
	for _, value := range strings.Split(c.GoogleReturnURLs, ",") {
		u, e := url.Parse(strings.TrimSpace(value))
		if e != nil || !safeOAuthURL(u, true) {
			return errors.New("invalid GOOGLE_RETURN_URLS entry")
		}
	}
	return nil
}

func safeOAuthURL(u *url.URL, app bool) bool {
	if u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || len(u.String()) > 512 {
		return false
	}
	if app && u.String() == "broto://auth-callback" {
		return true
	}
	return u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))
}

func newGoogleOAuth(c Config, client *http.Client) *googleOAuth {
	ctx := oidc.ClientContext(context.Background(), client)
	keys := oidc.NewRemoteKeySet(ctx, googleKeysURL)
	return &googleOAuth{
		config: oauth2.Config{ClientID: c.GoogleClientID, ClientSecret: c.GoogleClientSecret, RedirectURL: c.GoogleRedirectURL,
			Scopes: []string{oidc.ScopeOpenID, "email", "profile"}, Endpoint: oauth2.Endpoint{
				AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token", AuthStyle: oauth2.AuthStyleInParams}},
		verifier: oidc.NewVerifier(googleIssuer, keys, &oidc.Config{ClientID: c.GoogleClientID, SupportedSigningAlgs: []string{"RS256"}}), http: client,
	}
}

func challenge(v string) string {
	h := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func validChallenge(v string) bool {
	b, e := base64.RawURLEncoding.DecodeString(v)
	return e == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == v
}

func (s *Server) googleReady(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if s.google == nil {
		fail(w, 503, "google_nao_configurado")
		return false
	}
	if !s.allow("google:"+s.clientIP(r), 30) {
		fail(w, 429, "muitas_requisicoes")
		return false
	}
	return true
}

func (s *Server) googleStart(w http.ResponseWriter, r *http.Request) {
	if !s.googleReady(w, r) {
		return
	}
	var b struct {
		Challenge string `json:"code_challenge"`
		ReturnURI string `json:"redirect_uri"`
	}
	if !body(w, r, &b) {
		return
	}
	allowed := false
	for _, u := range strings.Split(s.C.GoogleReturnURLs, ",") {
		allowed = allowed || b.ReturnURI == strings.TrimSpace(u)
	}
	if !allowed || !validChallenge(b.Challenge) {
		fail(w, 400, "oauth_pedido_invalido")
		return
	}
	state := randomToken()
	_, e := s.DB.Exec(r.Context(), "insert into oauth_attempts(state_hash,app_challenge,return_uri) values($1,$2,$3)", hash(state), b.Challenge, b.ReturnURI)
	if e != nil {
		s.dbError(w, e)
		return
	}
	// The browser first visits our origin to bind a HttpOnly cookie. Native app
	// HTTP cookies do not necessarily share the browser's cookie jar.
	u, _ := url.Parse(s.C.GoogleRedirectURL)
	u.Path = "/v1/auth/google/authorize"
	u.RawQuery = url.Values{"state": {state}}.Encode()
	send(w, 200, map[string]any{"url": u.String(), "state": state, "expires_in": 600})
}

func oauthState(r *http.Request) (string, bool) {
	q, e := url.ParseQuery(r.URL.RawQuery)
	state := q.Get("state")
	if e != nil || len(q["state"]) != 1 || len(state) != 64 {
		return "", false
	}
	return state, true
}

func (s *Server) googleCookie(state, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: "broto_google_" + hash(state)[:24], Value: value, Path: "/v1/auth/google/callback",
		HttpOnly: true, Secure: strings.HasPrefix(s.C.GoogleRedirectURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: maxAge}
}

func (s *Server) googleAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.googleReady(w, r) {
		return
	}
	state, ok := oauthState(r)
	if !ok {
		fail(w, 400, "oauth_state_invalido")
		return
	}
	browser, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	tag, e := s.DB.Exec(r.Context(), `update oauth_attempts set browser_hash=$2,nonce_hash=$3,google_verifier=$4
		where state_hash=$1 and expires_at>now() and browser_hash is null`, hash(state), hash(browser), hash(nonce), verifier)
	if e != nil {
		s.dbError(w, e)
		return
	}
	if tag.RowsAffected() != 1 {
		fail(w, 400, "oauth_state_invalido")
		return
	}
	http.SetCookie(w, s.googleCookie(state, browser, 600))
	u := s.google.config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "select_account"))
	http.Redirect(w, r, u, http.StatusFound)
}

func googleReturn(w http.ResponseWriter, r *http.Request, target, state, code, failure string) {
	u, _ := url.Parse(target) // target was allowlisted and stored at start
	q := url.Values{"state": {state}}
	if failure != "" {
		q.Set("error", failure)
	} else {
		q.Set("code", code)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

type googleIdentity struct {
	Subject         string
	Email           string `json:"email"`
	EmailVerified   bool   `json:"email_verified"`
	Name            string `json:"name"`
	AuthorizedParty string `json:"azp"`
}

func (g *googleOAuth) identity(ctx context.Context, code, verifier, nonceHash string) (googleIdentity, error) {
	var identity googleIdentity
	ctx = context.WithValue(ctx, oauth2.HTTPClient, g.http)
	token, e := g.config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if e != nil {
		return identity, e
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || len(raw) > 16384 {
		return identity, errors.New("missing or oversized ID token")
	}
	id, e := g.verifier.Verify(ctx, raw)
	if e != nil {
		return identity, e
	}
	if subtle.ConstantTimeCompare([]byte(hash(id.Nonce)), []byte(nonceHash)) != 1 || id.Nonce == "" {
		return identity, errors.New("invalid nonce")
	}
	if e = id.Claims(&identity); e != nil {
		return identity, e
	}
	identity.Subject = id.Subject
	identity.Email = strings.ToLower(strings.TrimSpace(identity.Email))
	address, e := mail.ParseAddress(identity.Email)
	if e != nil || address.Address != identity.Email || !identity.EmailVerified || len(identity.Email) > 254 || id.Subject == "" || len(id.Subject) > 255 || len(identity.Name) > 512 {
		return identity, errors.New("invalid Google identity")
	}
	if (len(id.Audience) > 1 && identity.AuthorizedParty == "") || (identity.AuthorizedParty != "" && identity.AuthorizedParty != g.config.ClientID) {
		return identity, errors.New("invalid authorized party")
	}
	return identity, nil
}

var errGoogleEmailInUse = errors.New("Google account requires explicit linking or identity migration")

func googleUser(ctx context.Context, tx pgx.Tx, identity googleIdentity) (string, error) {
	// Serialize the same identity across replicas without merging by email.
	if _, e := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtextextended($1,2))", "google:"+identity.Subject); e != nil {
		return "", e
	}
	var id string
	e := tx.QueryRow(ctx, "select user_id::text from oauth_identities where provider='google' and subject=$1", identity.Subject).Scan(&id)
	if e == nil {
		return id, nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return "", e
	}
	meta, _ := json.Marshal(map[string]string{"name": identity.Name})
	e = tx.QueryRow(ctx, `insert into users(email,password_hash,email_confirmed_at,raw_user_meta_data)
		values($1,'!google-only',now(),$2) on conflict(email) do nothing returning id::text`, identity.Email, meta).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", errGoogleEmailInUse
	}
	if e != nil {
		return "", e
	}
	_, e = tx.Exec(ctx, "insert into oauth_identities(provider,subject,user_id) values('google',$1,$2)", identity.Subject, id)
	return id, e
}

func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	if !s.googleReady(w, r) {
		return
	}
	state, ok := oauthState(r)
	if !ok {
		fail(w, 400, "oauth_state_invalido")
		return
	}
	cookie, e := r.Cookie(s.googleCookie(state, "", 0).Name)
	if e != nil || len(cookie.Value) != 64 {
		fail(w, 400, "oauth_state_invalido")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var target, appChallenge, verifier, nonceHash string
	// Consume state BEFORE contacting Google: concurrent/replayed callbacks fail.
	e = s.DB.QueryRow(ctx, `delete from oauth_attempts where state_hash=$1 and browser_hash=$2 and expires_at>now()
		returning return_uri,app_challenge,google_verifier,nonce_hash`, hash(state), hash(cookie.Value)).Scan(&target, &appChallenge, &verifier, &nonceHash)
	if errors.Is(e, pgx.ErrNoRows) {
		fail(w, 400, "oauth_state_invalido")
		return
	}
	if e != nil {
		s.dbError(w, e)
		return
	}
	http.SetCookie(w, s.googleCookie(state, "", -1))
	q, _ := url.ParseQuery(r.URL.RawQuery)
	if q.Get("error") != "" {
		googleReturn(w, r, target, state, "", "google_login_cancelado")
		return
	}
	if len(q["code"]) != 1 || q.Get("code") == "" || len(q.Get("code")) > 4096 {
		googleReturn(w, r, target, state, "", "google_resposta_invalida")
		return
	}
	identity, e := s.google.identity(ctx, q.Get("code"), verifier, nonceHash)
	if e != nil {
		// Provider error strings may include credentials, codes or tokens.
		googleReturn(w, r, target, state, "", "google_autenticacao_falhou")
		return
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		googleReturn(w, r, target, state, "", "google_indisponivel")
		return
	}
	defer rollback(tx)
	id, e := googleUser(ctx, tx, identity)
	if errors.Is(e, errGoogleEmailInUse) {
		googleReturn(w, r, target, state, "", "google_email_em_uso")
		return
	}
	code := randomToken()
	if e == nil {
		_, e = tx.Exec(ctx, "insert into oauth_codes(code_hash,user_id,app_challenge) values($1,$2,$3)", hash(code), id, appChallenge)
	}
	if e == nil {
		e = tx.Commit(ctx)
	}
	if e != nil {
		googleReturn(w, r, target, state, "", "google_indisponivel")
		return
	}
	googleReturn(w, r, target, state, code, "")
}

func (s *Server) googleExchange(w http.ResponseWriter, r *http.Request) {
	if !s.googleReady(w, r) {
		return
	}
	var b struct {
		Code     string `json:"code"`
		Verifier string `json:"code_verifier"`
	}
	if !body(w, r, &b) {
		return
	}
	if len(b.Code) != 64 || !pkceVerifier.MatchString(b.Verifier) {
		fail(w, 400, "oauth_codigo_invalido")
		return
	}
	ctx := r.Context()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		s.dbError(w, e)
		return
	}
	defer rollback(tx)
	var id string
	e = tx.QueryRow(ctx, `delete from oauth_codes where code_hash=$1 and app_challenge=$2 and expires_at>now() returning user_id::text`, hash(b.Code), challenge(b.Verifier)).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		fail(w, 400, "oauth_codigo_invalido")
		return
	}
	if e != nil {
		s.dbError(w, e)
		return
	}
	v, e := s.session(ctx, tx, id)
	if e == nil {
		e = tx.Commit(ctx)
	}
	if e != nil {
		s.dbError(w, e)
		return
	}
	send(w, 200, v)
}
