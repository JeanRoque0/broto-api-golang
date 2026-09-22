package api

import (
	"context"
	"crypto/x509"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql assets/*
var assets embed.FS

type Server struct {
	DB             *pgxpool.Pool
	C              Config
	HTTP           *http.Client
	limiterMu      sync.Mutex
	limits         map[string]*window
	authSlots      chan struct{}
	smtpRoots      *x509.CertPool // nil uses system roots; tests trust only their ephemeral local CA
	google         *googleOAuth
	objects        objectStore
	cdn            *photoCDN
	trustedProxies []netip.Prefix
	requestSlots   chan struct{}
}
type window struct {
	start time.Time
	n     int
}
type userKey struct{}

func user(r *http.Request) string { v, _ := r.Context().Value(userKey{}).(string); return v }
func New(ctx context.Context, c Config) (*Server, error) {
	max := c.DBMaxConns
	if max == 0 {
		max = 10
	}
	db, e := openDatabase(ctx, c.DatabaseURL, int32(max))
	if e != nil {
		return nil, e
	}
	s := &Server{DB: db, C: c, HTTP: &http.Client{Timeout: 90 * time.Second}, limits: map[string]*window{}, authSlots: make(chan struct{}, 4)}
	s.cdn, e = newPhotoCDN(c.CDNURL, c.CDNKeyID, c.CDNPrivateKey)
	if e != nil {
		db.Close()
		return nil, e
	}
	s.trustedProxies, e = parseTrustedProxies(c.TrustedProxyCIDRs)
	if e != nil {
		db.Close()
		return nil, e
	}
	concurrent := c.MaxRequests
	if concurrent == 0 {
		concurrent = 24
	}
	s.requestSlots = make(chan struct{}, concurrent)
	if c.GoogleEnabled {
		if e = validateGoogleConfig(c); e != nil {
			db.Close()
			return nil, e
		}
		s.google = newGoogleOAuth(c, &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	}
	if c.DisableMigrations {
		e = s.checkMigrations(ctx)
	} else {
		e = s.migrate(ctx)
	}
	if e != nil {
		db.Close()
		return nil, e
	}
	if c.StorageBucket != "" {
		s.objects, e = newS3Objects(ctx, c)
	} else {
		e = os.MkdirAll(c.StorageDir, 0700)
	}
	if e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}
func (s *Server) migrate(ctx context.Context) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "select pg_advisory_xact_lock(730210); create table if not exists schema_migrations(name text primary key, applied_at timestamptz not null default now())"); e != nil {
		return e
	}
	files, _ := assets.ReadDir("migrations")
	for _, f := range files {
		var done bool
		if e = tx.QueryRow(ctx, "select exists(select 1 from schema_migrations where name=$1)", f.Name()).Scan(&done); e != nil {
			return e
		}
		if done {
			continue
		}
		b, _ := assets.ReadFile("migrations/" + f.Name())
		if _, e = tx.Exec(ctx, string(b)); e != nil {
			return fmt.Errorf("migration %s: %w", f.Name(), e)
		}
		if _, e = tx.Exec(ctx, "insert into schema_migrations(name) values($1)", f.Name()); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	send(w, status, map[string]any{"erro": msg})
}
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		fail(w, 400, "corpo_invalido")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		fail(w, 400, "corpo_invalido")
		return false
	}
	return true
}
func (s *Server) dbError(w http.ResponseWriter, e error) {
	var pg *pgconn.PgError
	if errors.As(e, &pg) && (strings.HasPrefix(pg.Code, "22") || strings.HasPrefix(pg.Code, "23")) {
		fail(w, 400, "dados_invalidos_ou_conflito")
		return
	}
	slog.Error("database operation failed", "error", e)
	fail(w, 500, "falha_banco")
}

func (s *Server) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var id string
		if token == "" || token == r.Header.Get("Authorization") {
			fail(w, 401, "sem_token")
			return
		}
		e := s.DB.QueryRow(r.Context(), "select user_id::text from sessions where token_hash=$1 and expires_at>now()", hash(token)).Scan(&id)
		if errors.Is(e, pgx.ErrNoRows) {
			fail(w, 401, "sem_token")
			return
		}
		if e != nil {
			slog.Error("session lookup failed", "error", e)
			fail(w, 503, "banco_indisponivel")
			return
		}
		if !s.allow("u:"+id, 120) {
			fail(w, 429, "muitas_requisicoes")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, id)))
	}
}
func (s *Server) allow(key string, max int) bool {
	s.limiterMu.Lock()
	defer s.limiterMu.Unlock()
	now := time.Now()
	if len(s.limits) > 10000 {
		for k, v := range s.limits {
			if now.Sub(v.start) > time.Minute {
				delete(s.limits, k)
			}
		}
		if len(s.limits) > 10000 {
			return false
		}
	}
	v := s.limits[key]
	if v == nil || now.Sub(v.start) > time.Minute {
		v = &window{start: now}
		s.limits[key] = v
	}
	v.n++
	return v.n <= max
}
func (s *Server) Routes() http.Handler {
	m := http.NewServeMux()
	registerDocumentation(m)
	m.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { send(w, 200, map[string]bool{"ok": true}) })
	ready := func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.DB.Ping(ctx) != nil {
			fail(w, 503, "banco_indisponivel")
			return
		}
		send(w, 200, map[string]bool{"ok": true})
	}
	m.HandleFunc("GET /healthz", ready)
	m.HandleFunc("GET /readyz", ready)
	for _, path := range []string{"signup", "login", "recover", "resend", "verify"} {
		m.HandleFunc("POST /v1/auth/"+path, s.auth)
	}
	m.HandleFunc("POST /v1/auth/google/start", s.googleStart)
	m.HandleFunc("GET /v1/auth/google/authorize", s.googleAuthorize)
	m.HandleFunc("GET /v1/auth/google/callback", s.googleCallback)
	m.HandleFunc("POST /v1/auth/google/exchange", s.googleExchange)
	m.HandleFunc("POST /v1/auth/logout", s.protect(s.logout))
	m.HandleFunc("POST /v1/auth/refresh", s.protect(s.refresh))
	m.HandleFunc("PATCH /v1/auth/password", s.protect(s.password))
	m.HandleFunc("GET /v1/auth/user", s.protect(s.me))
	m.HandleFunc("GET /v1/species-facts", s.protect(s.getSpeciesFacts))
	m.HandleFunc("GET /v1/reminders/unread-count", s.protect(s.unreadReminders))
	m.HandleFunc("POST /v1/reminders/read", s.protect(s.readReminders))
	for _, path := range []string{"/v1/identify", "/functions/v1/identify"} {
		m.HandleFunc("POST "+path, s.protect(s.identify))
	}
	for _, path := range []string{"/v1/chat", "/functions/v1/chat"} {
		m.HandleFunc("POST "+path, s.protect(s.chat))
	}
	for _, path := range []string{"/v1/search", "/functions/v1/search"} {
		m.HandleFunc("POST "+path, s.protect(s.search))
	}
	m.HandleFunc("DELETE /v1/account", s.protect(s.deleteAccount))
	m.HandleFunc("POST /functions/v1/delete-account", s.protect(s.deleteAccount))
	m.HandleFunc("POST /v1/photos", s.protect(s.upload))
	m.HandleFunc("POST /v1/photos/sign", s.protect(s.signPhoto))
	m.HandleFunc("DELETE /v1/photos", s.protect(s.deletePhoto))
	m.HandleFunc("GET /v1/photos/object", s.photo)
	m.HandleFunc("/v1/data/{resource}", s.protect(s.data))
	m.HandleFunc("/v1/data/{resource}/{id}", s.protect(s.data))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/livez" && r.URL.Path != "/readyz" && r.URL.Path != "/healthz" && s.requestSlots != nil {
			select {
			case s.requestSlots <- struct{}{}:
				defer func() { <-s.requestSlots }()
			default:
				w.Header().Set("Retry-After", "1")
				fail(w, 503, "servidor_ocupado")
				return
			}
		}
		origin := r.Header.Get("Origin")
		if origin != "" {
			ok := origin == publicOrigin(s.C.PublicURL)
			for _, v := range strings.Split(s.C.Origins, ",") {
				if strings.TrimSpace(v) == origin {
					ok = true
				}
			}
			if !ok {
				fail(w, 403, "origem_nao_permitida")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		defer func() {
			if recover() != nil {
				slog.Error("request panic")
				fail(w, 500, "erro_interno")
			}
		}()
		m.ServeHTTP(w, r)
	})
}
func remoteIP(r *http.Request) string {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return r.RemoteAddr
	}
	return host
}
func rowJSON(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, q string, args ...any) (json.RawMessage, error) {
	var b []byte
	e := db.QueryRow(ctx, q, args...).Scan(&b)
	return b, e
}
