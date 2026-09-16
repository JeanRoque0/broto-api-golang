package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
)

func randomToken() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// Match the app's Unicode-aware password checklist; bcrypt is limited to 72 bytes.
func validPassword(s string) bool {
	if len(s) > 72 || len(utf16.Encode([]rune(s))) < 8 {
		return false
	}
	upper, special := false, false
	for _, r := range s {
		upper = upper || unicode.IsUpper(r)
		special = special || (!unicode.IsLetter(r) && !unicode.IsNumber(r))
	}
	return upper && special
}
func (s *Server) session(ctx context.Context, tx pgx.Tx, id string) (map[string]any, error) {
	t := randomToken()
	var expires time.Time
	e := tx.QueryRow(ctx, "insert into sessions(token_hash,user_id,expires_at) values($1,$2,now()+interval '30 days') returning expires_at", hash(t), id).Scan(&expires)
	if e != nil {
		return nil, e
	}
	u, e := rowJSON(ctx, tx, "select jsonb_build_object('id',id,'email',email,'email_confirmed_at',email_confirmed_at,'user_metadata',raw_user_meta_data) from users where id=$1", id)
	if e != nil {
		return nil, e
	}
	return map[string]any{"access_token": t, "token_type": "bearer", "expires_in": 2592000, "expires_at": expires.Unix(), "user": u}, nil
}
func (s *Server) auth(w http.ResponseWriter, r *http.Request) {
	if !s.allow("auth:"+s.clientIP(r), 15) {
		fail(w, 429, "muitas_requisicoes")
		return
	}
	select {
	case s.authSlots <- struct{}{}:
		defer func() { <-s.authSlots }()
	default:
		fail(w, 429, "muitas_requisicoes")
		return
	}
	var b struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
		Token    string `json:"token_hash"`
		Type     string `json:"type"`
	}
	if !body(w, r, &b) {
		return
	}
	ctx := r.Context()
	action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if action == "verify" {
		s.verify(w, r, b.Token, b.Type)
		return
	}
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	addr, e := mail.ParseAddress(b.Email)
	if e != nil || addr.Address != b.Email || len(b.Email) > 254 {
		fail(w, 400, "email_invalido")
		return
	}
	if action == "recover" || action == "resend" {
		var id string
		e = s.DB.QueryRow(ctx, "select id::text from users where email=$1", b.Email).Scan(&id)
		if e != nil && e != pgx.ErrNoRows {
			s.dbError(w, e)
			return
		}
		if e == nil {
			kind := "recovery"
			if action == "resend" {
				kind = "email"
			}
			if e = s.authEmail(ctx, id, b.Email, kind); e != nil {
				if s.emailIntervalError(w, e) {
					return
				}
				fail(w, 503, "email_indisponivel")
				return
			}
		}
		send(w, 200, map[string]bool{"ok": true})
		return
	}
	if (action == "signup" && !validPassword(b.Password)) || b.Password == "" || len(b.Password) > 72 {
		fail(w, 400, "senha_invalida")
		return
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		s.dbError(w, e)
		return
	}
	defer tx.Rollback(ctx)
	var id string
	if action == "signup" {
		p, e := bcrypt.GenerateFromPassword([]byte(b.Password), 12)
		if e != nil {
			fail(w, 500, "falha_senha")
			return
		}
		meta, _ := json.Marshal(map[string]string{"name": b.Name})
		e = tx.QueryRow(ctx, "insert into users(email,password_hash,raw_user_meta_data,email_confirmed_at) values($1,$2,$3,case when $4 then now() end) on conflict(email) do nothing returning id::text", b.Email, string(p), meta, s.C.DevAuth).Scan(&id)
		if e == pgx.ErrNoRows {
			send(w, 202, map[string]bool{"confirmation_required": true})
			return
		}
		if e != nil {
			s.dbError(w, e)
			return
		}
		if !s.C.DevAuth {
			if e = tx.Commit(ctx); e != nil {
				s.dbError(w, e)
				return
			}
			if e = s.authEmail(ctx, id, b.Email, "email"); e != nil {
				if s.emailIntervalError(w, e) {
					return
				}
				fail(w, 503, "email_indisponivel_use_recover")
				return
			}
			send(w, 202, map[string]bool{"confirmation_required": true})
			return
		}
	} else {
		var p string
		var confirmed *time.Time
		e = tx.QueryRow(ctx, "select id::text,password_hash,email_confirmed_at from users where email=$1", b.Email).Scan(&id, &p, &confirmed)
		if e != nil {
			p = "$2a$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW"
		}
		passwordError := bcrypt.CompareHashAndPassword([]byte(p), []byte(b.Password))
		if e != nil || passwordError != nil {
			fail(w, 401, "credenciais_invalidas")
			return
		}
		if confirmed == nil {
			fail(w, 403, "confirme_email")
			return
		}
	}
	session, e := s.session(ctx, tx, id)
	if e == nil {
		e = tx.Commit(ctx)
	}
	if e != nil {
		s.dbError(w, e)
		return
	}
	send(w, 200, session)
}

var errEmailInterval = errors.New("minimum SMTP interval not elapsed")

func (s *Server) emailIntervalError(w http.ResponseWriter, e error) bool {
	if !errors.Is(e, errEmailInterval) {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(s.C.SMTPMinIntervalSeconds))
	fail(w, 429, "intervalo_email")
	return true
}

func (s *Server) authEmail(ctx context.Context, id, email, kind string) error {
	token := randomToken()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	if s.C.SMTPMinIntervalSeconds > 0 {
		tag, e := tx.Exec(ctx, `insert into auth_email_sends(user_id,attempted_at) values($1,clock_timestamp())
			on conflict(user_id) do update set attempted_at=excluded.attempted_at
			where auth_email_sends.attempted_at<=clock_timestamp()-make_interval(secs=>$2)`, id, s.C.SMTPMinIntervalSeconds)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return errEmailInterval
		}
	}
	_, e = tx.Exec(ctx, "insert into auth_tokens(token_hash,user_id,kind,expires_at) values($1,$2,$3,now()+interval '1 hour')", hash(token), id, kind)
	if e == nil {
		e = tx.Commit(ctx)
	}
	if e != nil {
		return e
	}
	path := "/confirmado"
	if kind == "recovery" {
		path = "/nova-senha"
	}
	link := strings.TrimRight(s.C.SiteURL, "/") + path + "?token_hash=" + url.QueryEscape(token) + "&type=" + kind
	return sendSMTP(ctx, s.C, email, link, s.smtpRoots)
}
func (s *Server) verify(w http.ResponseWriter, r *http.Request, token, kind string) {
	if len(token) != 64 || (kind != "email" && kind != "recovery") {
		fail(w, 400, "token_invalido")
		return
	}
	ctx := r.Context()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		s.dbError(w, e)
		return
	}
	defer tx.Rollback(ctx)
	var id string
	e = tx.QueryRow(ctx, "delete from auth_tokens where token_hash=$1 and kind=$2 and expires_at>now() returning user_id::text", hash(token), kind).Scan(&id)
	if e != nil {
		fail(w, 400, "token_invalido")
		return
	}
	if _, e = tx.Exec(ctx, "update users set email_confirmed_at=coalesce(email_confirmed_at,now()) where id=$1", id); e != nil {
		s.dbError(w, e)
		return
	}
	if kind == "recovery" {
		if _, e = tx.Exec(ctx, "delete from sessions where user_id=$1", id); e != nil {
			s.dbError(w, e)
			return
		}
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
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	b, e := rowJSON(r.Context(), s.DB, "select jsonb_build_object('id',id,'email',email,'email_confirmed_at',email_confirmed_at,'user_metadata',raw_user_meta_data) from users where id=$1", user(r))
	if e != nil {
		s.dbError(w, e)
		return
	}
	send(w, 200, b)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	_, e := s.DB.Exec(r.Context(), "delete from sessions where token_hash=$1", hash(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	if e != nil {
		s.dbError(w, e)
		return
	}
	send(w, 200, map[string]bool{"ok": true})
}
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		s.dbError(w, e)
		return
	}
	defer tx.Rollback(ctx)
	tag, e := tx.Exec(ctx, "delete from sessions where token_hash=$1 and expires_at>now()", hash(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	if e != nil || tag.RowsAffected() != 1 {
		fail(w, 401, "sem_token")
		return
	}
	v, e := s.session(ctx, tx, user(r))
	if e == nil {
		e = tx.Commit(ctx)
	}
	if e != nil {
		s.dbError(w, e)
		return
	}
	send(w, 200, v)
}
func (s *Server) password(w http.ResponseWriter, r *http.Request) {
	if !s.allow("password:"+user(r), 5) {
		fail(w, 429, "muitas_requisicoes")
		return
	}
	select {
	case s.authSlots <- struct{}{}:
		defer func() { <-s.authSlots }()
	default:
		fail(w, 429, "muitas_requisicoes")
		return
	}
	var b struct {
		Password string `json:"password"`
	}
	if !body(w, r, &b) {
		return
	}
	if !validPassword(b.Password) {
		fail(w, 400, "senha_invalida")
		return
	}
	p, e := bcrypt.GenerateFromPassword([]byte(b.Password), 12)
	if e != nil {
		fail(w, 500, "falha_senha")
		return
	}
	ctx := r.Context()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		s.dbError(w, e)
		return
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "update users set password_hash=$1 where id=$2", string(p), user(r)); e == nil {
		_, e = tx.Exec(ctx, "delete from sessions where user_id=$1", user(r))
	}
	var v map[string]any
	if e == nil {
		v, e = s.session(ctx, tx, user(r))
	}
	if e == nil {
		e = tx.Commit(ctx)
	}
	if e != nil {
		s.dbError(w, e)
		return
	}
	send(w, 200, v)
}
