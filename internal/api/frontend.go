package api

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// getSpeciesFacts matches services/supabase/speciesFacts.ts: cached data or null.
// Species knowledge is shared; no user-owned records are exposed by this route.
func (s *Server) getSpeciesFacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for key, vs := range q {
		if (key != "scientific" && key != "language") || len(vs) != 1 {
			fail(w, 400, "filtro_invalido")
			return
		}
	}
	scientific := strings.TrimSpace(q.Get("scientific"))
	language := q.Get("language")
	if scientific == "" || len(scientific) > 200 || !contains("pt-BR en-US es-ES", language) {
		fail(w, 400, "especie_ou_idioma_invalido")
		return
	}
	data, e := rowJSON(r.Context(), s.DB, "select data from species_facts where scientific=$1 and language=$2", scientific, language)
	if e == pgx.ErrNoRows {
		send(w, 200, nil)
		return
	}
	if e != nil {
		s.dbError(w, e)
		return
	}
	send(w, 200, data)
}

// Counts must not depend on the app's 60-row inbox window.
func (s *Server) unreadReminders(w http.ResponseWriter, r *http.Request) {
	var count int64
	if e := s.DB.QueryRow(r.Context(), "select count(*) from reminder_events where user_id=$1 and read_at is null", user(r)).Scan(&count); e != nil {
		s.dbError(w, e)
		return
	}
	send(w, 200, map[string]int64{"count": count})
}
func (s *Server) readReminders(w http.ResponseWriter, r *http.Request) {
	tag, e := s.DB.Exec(r.Context(), "update reminder_events set read_at=now() where user_id=$1 and read_at is null", user(r))
	if e != nil {
		s.dbError(w, e)
		return
	}
	send(w, 200, map[string]any{"ok": true, "updated": tag.RowsAffected()})
}
