package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var safePath = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)

func ownsPath(path, id string) bool {
	return strings.HasPrefix(path, id+"/") && safePath.MatchString(path) && !strings.Contains(path, "..") && !strings.Contains(path, "//") && !strings.HasSuffix(path, "/") && len(path) <= 512
}
func (s *Server) filePath(path string) string {
	return filepath.Join(s.C.StorageDir, filepath.FromSlash(path))
}
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	b, e := io.ReadAll(r.Body)
	if e != nil || len(b) == 0 {
		fail(w, 400, "foto_invalida_maximo_8mb")
		return
	}
	mime := http.DetectContentType(b)
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"}[mime]
	if ext == "" {
		fail(w, 400, "formato_invalido")
		return
	}
	id := user(r)
	path := id + "/" + randomToken() + ext
	ctx := r.Context()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		s.dbError(w, e)
		return
	}
	defer tx.Rollback(ctx)
	// Serialize quota checks with writes across all API replicas sharing storage.
	if _, e = tx.Exec(ctx, "select pg_advisory_xact_lock(730211)"); e != nil {
		s.dbError(w, e)
		return
	}
	var used int64
	if e = tx.QueryRow(ctx, "select coalesce(sum(size),0) from stored_files").Scan(&used); e != nil {
		s.dbError(w, e)
		return
	}
	if used+int64(len(b)) > s.C.StorageQuota {
		fail(w, 507, "armazenamento_cheio")
		return
	}
	if e = s.putPhoto(ctx, path, b, mime); e != nil {
		_ = tx.Rollback(ctx)
		s.discardPhoto(path)
		fail(w, 500, "falha_storage")
		return
	}
	_, e = tx.Exec(ctx, "insert into stored_files(path,user_id,size,media_type) values($1,$2,$3,$4)", path, id, len(b), mime)
	if e == nil {
		e = tx.Commit(ctx)
	}
	if e != nil {
		_ = tx.Rollback(ctx)
		s.discardPhoto(path)
		s.dbError(w, e)
		return
	}
	send(w, 201, map[string]string{"path": path})
}
func (s *Server) signature(path, exp string) string {
	m := hmac.New(sha256.New, []byte(s.C.SigningKey))
	_, _ = fmt.Fprintf(m, "%s\n%s", path, exp)
	return hex.EncodeToString(m.Sum(nil))
}
func (s *Server) signPhoto(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Path string `json:"path"`
	}
	if !body(w, r, &b) {
		return
	}
	if !ownsPath(b.Path, user(r)) {
		fail(w, 403, "foto_de_outro_usuario")
		return
	}
	var exists bool
	if e := s.DB.QueryRow(r.Context(), "select exists(select 1 from stored_files where path=$1 and user_id=$2)", b.Path, user(r)).Scan(&exists); e != nil {
		s.dbError(w, e)
		return
	}
	if !exists {
		fail(w, 404, "foto_invalida")
		return
	}
	if s.cdn != nil {
		signed, e := s.cdn.signedURL(b.Path, time.Now().Add(5*time.Minute))
		if e != nil {
			fail(w, 500, "falha_assinatura")
			return
		}
		send(w, 200, map[string]any{"signedUrl": signed, "expiresIn": 300})
		return
	}
	exp := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	q := url.Values{"path": {b.Path}, "expires": {exp}, "signature": {s.signature(b.Path, exp)}}
	send(w, 200, map[string]any{"signedUrl": strings.TrimRight(s.C.PublicURL, "/") + "/v1/photos/object?" + q.Encode(), "expiresIn": 3600})
}
func (s *Server) photo(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	exp := q.Get("expires")
	n, e := strconv.ParseInt(exp, 10, 64)
	id, _, _ := strings.Cut(path, "/")
	sig, e2 := hex.DecodeString(q.Get("signature"))
	want, _ := hex.DecodeString(s.signature(path, exp))
	if e != nil || e2 != nil || n < time.Now().Unix() || n > time.Now().Add(time.Hour+time.Minute).Unix() || !ownsPath(path, id) || !hmac.Equal(sig, want) {
		fail(w, 403, "assinatura_invalida")
		return
	}
	var mime string
	if s.DB.QueryRow(r.Context(), "select media_type from stored_files where path=$1", path).Scan(&mime) != nil {
		fail(w, 404, "foto_invalida")
		return
	}
	f, e := s.openPhoto(r.Context(), path)
	if e != nil {
		fail(w, 404, "foto_invalida")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = io.Copy(w, f)
}
func (s *Server) deletePhoto(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Path string `json:"path"`
	}
	if !body(w, r, &b) {
		return
	}
	if !ownsPath(b.Path, user(r)) {
		fail(w, 403, "foto_de_outro_usuario")
		return
	}
	var used bool
	e := s.DB.QueryRow(r.Context(), "select exists(select 1 from plants where photo_path=$1 union all select 1 from profiles where avatar_path=$1 union all select 1 from identifications where photo_path=$1)", b.Path).Scan(&used)
	if e != nil {
		s.dbError(w, e)
		return
	}
	if used {
		fail(w, 409, "foto_em_uso")
		return
	}
	if _, e = s.DB.Exec(r.Context(), "delete from stored_files where path=$1 and user_id=$2", b.Path, user(r)); e != nil {
		s.dbError(w, e)
		return
	}
	if e = s.drainFiles(r.Context()); e != nil {
		fail(w, 500, "falha_storage")
		return
	}
	send(w, 200, map[string]bool{"ok": true})
}
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id := user(r)
	if _, e := s.DB.Exec(r.Context(), "delete from users where id=$1", id); e != nil {
		s.dbError(w, e)
		return
	}
	if e := s.drainFiles(r.Context()); e != nil {
		send(w, 202, map[string]any{"ok": true, "photos_cleanup_pending": true})
		return
	}
	send(w, 200, map[string]bool{"ok": true})
}
