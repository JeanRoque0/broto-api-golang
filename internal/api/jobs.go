package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func (s *Server) RunJobs(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runJobs(ctx)
		}
	}
}
func (s *Server) runJobs(parent context.Context) {
	for _, job := range []struct {
		name string
		run  func(context.Context, pgx.Tx) error
	}{
		{"cleanup", s.cleanup}, {"reminders", s.reminders},
	} {
		ctx, cancel := context.WithTimeout(parent, 50*time.Second)
		e := s.runJob(ctx, job.name, job.run)
		cancel()
		if e != nil {
			slog.Error("scheduled job failed", "job", job.name, "error", e)
		}
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if e := s.drainFiles(ctx); e != nil {
		slog.Error("file cleanup failed", "error", e)
	}
}
func (s *Server) runJob(ctx context.Context, name string, run func(context.Context, pgx.Tx) error) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	var lock bool
	if e = tx.QueryRow(ctx, "select pg_try_advisory_xact_lock(hashtextextended($1,1))", name).Scan(&lock); e != nil {
		return e
	}
	if !lock {
		return nil
	}
	var due bool
	if e = tx.QueryRow(ctx, "select not exists(select 1 from job_runs where name=$1 and completed_at>=date_trunc('hour',now()))", name).Scan(&due); e != nil {
		return e
	}
	if !due {
		return nil
	}
	if e = run(ctx, tx); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "insert into job_runs(name,completed_at) values($1,now()) on conflict(name) do update set completed_at=excluded.completed_at", name); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Server) drainFiles(ctx context.Context) error {
	rows, e := s.DB.Query(ctx, "select path from file_deletions order by queued_at limit 100")
	if e != nil {
		return e
	}
	paths := []string{}
	for rows.Next() {
		var p string
		if e = rows.Scan(&p); e != nil {
			rows.Close()
			return e
		}
		paths = append(paths, p)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, p := range paths {
		id, _, _ := strings.Cut(p, "/")
		if !ownsPath(p, id) {
			return fmt.Errorf("invalid stored path")
		}
		if e = s.removePhoto(ctx, p); e != nil {
			return e
		}
		if _, e = s.DB.Exec(ctx, "delete from file_deletions where path=$1", p); e != nil {
			return e
		}
	}
	return nil
}
func (s *Server) cleanup(ctx context.Context, tx pgx.Tx) error {
	if _, e := tx.Exec(ctx, "delete from oauth_attempts where expires_at<now(); delete from oauth_codes where expires_at<now()"); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, "delete from identifications where plant_id is null and created_at<now()-interval '7 days'; select purge_stale_chat_threads(); select purge_reminder_events(); delete from sessions where expires_at<now(); delete from auth_tokens where expires_at<now()"); e != nil {
		return e
	}
	_, e := tx.Exec(ctx, `delete from stored_files where path in (
 select path from stored_files f where created_at<now()-interval '7 days'
 and not exists(select 1 from plants where photo_path=f.path)
 and not exists(select 1 from profiles where avatar_path=f.path)
 and not exists(select 1 from identifications where photo_path=f.path) limit 100)`)
	return e
}

type reminder struct {
	User              string
	Plant             *string
	Kind, Title, Body string
}

var taskNames = map[string][]string{"pt-BR": {"Regar", "Adubar", "Vaporizar", "Girar o vaso", "Trocar de vaso", "Podar", "Analisar de novo"}, "en-US": {"Water", "Fertilise", "Mist", "Turn the pot", "Repot", "Prune", "Analyse again"}, "es-ES": {"Regar", "Abonar", "Vaporizar", "Girar la maceta", "Cambiar de maceta", "Podar", "Analizar de nuevo"}}

func reminderBody(lang string, kinds []string, late int) string {
	names, ok := taskNames[lang]
	if !ok {
		lang = "pt-BR"
		names = taskNames[lang]
	}
	labels := []string{}
	for i, k := range []string{"water", "fertilize", "mist", "rotate", "repot", "prune", "recheck"} {
		for _, kind := range kinds {
			if k == kind {
				labels = append(labels, names[i])
				break
			}
		}
	}
	and := map[string]string{"pt-BR": "e", "en-US": "and", "es-ES": "y"}[lang]
	list := strings.Join(labels, ", ")
	if len(labels) > 1 {
		list = strings.Join(labels[:len(labels)-1], ", ") + " " + and + " " + labels[len(labels)-1]
	}
	if late == 0 {
		return map[string]string{"pt-BR": "Hoje: ", "en-US": "Today: ", "es-ES": "Hoy: "}[lang] + list + "."
	}
	if lang == "en-US" {
		if late == 1 {
			return "1 day late: " + list + "."
		}
		return fmt.Sprintf("%d days late: %s.", late, list)
	}
	if lang == "es-ES" {
		if late == 1 {
			return "Atrasada 1 día: " + list + "."
		}
		return fmt.Sprintf("Atrasada %d días: %s.", late, list)
	}
	if late == 1 {
		return "Atrasada há 1 dia: " + list + "."
	}
	return fmt.Sprintf("Atrasada há %d dias: %s.", late, list)
}
func (s *Server) reminders(ctx context.Context, tx pgx.Tx) error {
	rows, e := tx.Query(ctx, "select * from due_reminders()")
	if e != nil {
		return e
	}
	out := []reminder{}
	for rows.Next() {
		var v reminder
		var lang string
		var kinds []string
		var late int
		if e = rows.Scan(&v.User, &v.Plant, &v.Title, &lang, &kinds, &late); e != nil {
			rows.Close()
			return e
		}
		v.Kind = "care"
		if late > 0 {
			v.Kind = "late"
		}
		v.Body = reminderBody(lang, kinds, late)
		out = append(out, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	rows, e = tx.Query(ctx, "select * from due_chat_nudges(5)")
	if e != nil {
		return e
	}
	for rows.Next() {
		var id, lang string
		if e = rows.Scan(&id, &lang); e != nil {
			rows.Close()
			return e
		}
		title, text := "Alguma dúvida?", "O Brotinho conhece as suas plantas e responde sobre elas."
		if lang == "en-US" {
			title, text = "Any doubts?", "Brotinho knows your plants and answers about them."
		} else if lang == "es-ES" {
			title, text = "¿Alguna duda?", "Brotinho conoce tus plantas y responde sobre ellas."
		}
		out = append(out, reminder{User: id, Kind: "chat", Title: title, Body: text})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, v := range out {
		var exists bool
		if e = tx.QueryRow(ctx, "select exists(select 1 from reminder_events where user_id=$1 and plant_id is not distinct from $2::uuid and kind=$3 and sent_at>=date_trunc('hour',now()))", v.User, v.Plant, v.Kind).Scan(&exists); e != nil {
			return e
		}
		if exists {
			continue
		}
		tokens, e := tx.Query(ctx, "select token from push_tokens where user_id=$1", v.User)
		if e != nil {
			return e
		}
		list := []string{}
		for tokens.Next() {
			var token string
			if e = tokens.Scan(&token); e != nil {
				tokens.Close()
				return e
			}
			list = append(list, token)
		}
		e = tokens.Err()
		tokens.Close()
		if e != nil {
			return e
		}
		sent := false
		for _, token := range list {
			payload, _ := json.Marshal([]any{map[string]any{"to": token, "title": v.Title, "body": v.Body, "data": map[string]any{"kind": v.Kind, "plantId": v.Plant}}})
			req, e := http.NewRequestWithContext(ctx, "POST", "https://exp.host/--/api/v2/push/send", bytes.NewReader(payload))
			if e != nil {
				return e
			}
			req.Header.Set("Content-Type", "application/json")
			res, e := s.HTTP.Do(req)
			if e != nil {
				return e
			}
			var tickets struct {
				Data []struct {
					Status  string `json:"status"`
					Details struct {
						Error string `json:"error"`
					} `json:"details"`
				} `json:"data"`
			}
			e = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&tickets)
			res.Body.Close()
			if res.StatusCode != 200 || e != nil {
				return fmt.Errorf("push provider failed")
			}
			for _, t := range tickets.Data {
				if t.Status == "ok" {
					sent = true
				}
				if t.Details.Error == "DeviceNotRegistered" {
					if _, e = tx.Exec(ctx, "delete from push_tokens where token=$1", token); e != nil {
						return e
					}
				}
			}
		}
		if sent {
			if _, e = tx.Exec(ctx, "insert into reminder_events(user_id,plant_id,kind,title,body) values($1,$2,$3,$4,$5)", v.User, v.Plant, v.Kind, v.Title, v.Body); e != nil {
				return e
			}
		}
	}
	return nil
}
