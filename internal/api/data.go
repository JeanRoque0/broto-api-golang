package api

import (
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type resource struct{ key, owner, fields, methods, order string }

var resources = map[string]resource{
	"profiles":         {"id", "id", "display_name avatar_path timezone reminder_time notifications_enabled accepted_terms_at terms_version accepted_tips accepted_tips_at temperature_unit dismissed_announcement revoked_terms_at language last_seen_at", "GET PATCH", "created_at"},
	"plants":           {"id", "user_id", "nickname species_scientific species_common photo_path room watering_interval_days light care_notes last_watered_at notify_watering archived_at group_id fertilizer light_note fertilizer_note toxic_to_pets mist_days rotate_days repot_months prune_month temp_min_c temp_max_c", "GET POST PATCH DELETE", "created_at"},
	"plant_groups":     {"id", "user_id", "name", "GET POST PATCH DELETE", "created_at"},
	"plant_tasks":      {"id", "user_id", "plant_id kind interval_days next_at enabled remind_at", "GET POST PATCH DELETE", "next_at"},
	"care_events":      {"id", "user_id", "plant_id kind note happened_at", "GET POST PATCH DELETE", "happened_at"},
	"identifications":  {"id", "user_id", "plant_id corrected_species was_helpful resolved_at", "GET PATCH DELETE", "created_at"},
	"chat_threads":     {"id", "user_id", "", "GET DELETE", "last_message_at"},
	"chat_messages":    {"id", "user_id", "", "GET", "created_at"},
	"push_tokens":      {"token", "user_id", "token platform", "GET POST PATCH DELETE", "updated_at"},
	"reminder_events":  {"id", "user_id", "read_at", "GET PATCH", "sent_at"},
	"announcements":    {"id", "", "", "GET", "starts_at"},
	"ad_rewards":       {"transaction_id", "user_id", "", "GET", "credited_at"},
	"credit_purchases": {"transaction_id", "user_id", "", "GET", "credited_at"},
}

func contains(fields, k string) bool {
	for _, v := range strings.Fields(fields) {
		if v == k {
			return true
		}
	}
	return false
}
func (s *Server) data(w http.ResponseWriter, r *http.Request) {
	query, queryError := url.ParseQuery(r.URL.RawQuery)
	if queryError != nil {
		fail(w, 400, "filtro_invalido")
		return
	}
	name := r.PathValue("resource")
	spec, ok := resources[name]
	if !ok {
		fail(w, 404, "recurso_invalido")
		return
	}
	if !contains(spec.methods, r.Method) {
		fail(w, 405, "metodo")
		return
	}
	id := r.PathValue("id")
	args := []any{}
	where := "true"
	if spec.owner != "" {
		args = append(args, user(r))
		where = spec.owner + "=$1"
	}
	if name == "profiles" {
		if id != "" && id != user(r) {
			fail(w, 404, "nao_encontrado")
			return
		}
		id = user(r)
	}
	if name == "announcements" {
		where = "starts_at<=now() and (ends_at is null or ends_at>now())"
	}
	if id != "" {
		args = append(args, id)
		where += fmt.Sprintf(" and %s=$%d", spec.key, len(args))
	}
	ctx := r.Context()
	var q string
	switch r.Method {
	case "GET":
		direction := query.Get("order")
		if direction == "" {
			direction = "desc"
		}
		if direction != "asc" && direction != "desc" {
			fail(w, 400, "ordem_invalida")
			return
		}
		for k, vs := range query {
			if len(vs) != 1 {
				fail(w, 400, "filtro_invalido")
				return
			}
			if k == "active" && name == "plant_tasks" {
				if vs[0] != "true" {
					fail(w, 400, "filtro_invalido")
					return
				}
				where += " and plant_id in (select id from plants where archived_at is null)"
				continue
			}
			if k == "limit" || k == "offset" || k == "order" {
				continue
			}
			if !contains(spec.fields+" "+spec.key+" plant_id thread_id", k) || len(vs) != 1 {
				fail(w, 400, "filtro_invalido")
				return
			}
			if (k == "plant_id" && !contains("plant_tasks care_events identifications chat_threads reminder_events", name)) || (k == "thread_id" && name != "chat_messages") {
				fail(w, 400, "filtro_invalido")
				return
			}
			if vs[0] == "null" {
				where += " and " + k + " is null"
				continue
			}
			args = append(args, vs[0])
			where += fmt.Sprintf(" and %s=$%d", k, len(args))
		}
		limit := 100
		offset := 0
		var e error
		if v := query.Get("limit"); v != "" {
			limit, e = strconv.Atoi(v)
			if e != nil || limit < 1 || limit > 200 {
				fail(w, 400, "limite_invalido")
				return
			}
		}
		if v := query.Get("offset"); v != "" {
			offset, e = strconv.Atoi(v)
			if e != nil || offset < 0 {
				fail(w, 400, "offset_invalido")
				return
			}
		}
		q = fmt.Sprintf("select coalesce(jsonb_agg(to_jsonb(t)),'[]') from (select * from %s where %s order by %s %s,%s %s limit %d offset %d) t", name, where, spec.order, direction, spec.key, direction, limit, offset)
		if id != "" {
			q = fmt.Sprintf("select to_jsonb(t) from %s t where %s", name, where)
		}
		b, e := rowJSON(ctx, s.DB, q, args...)
		if e == pgx.ErrNoRows {
			fail(w, 404, "nao_encontrado")
			return
		}
		if e != nil {
			s.dbError(w, e)
			return
		}
		send(w, 200, b)
	case "DELETE":
		if id == "" {
			fail(w, 400, "id_obrigatorio")
			return
		}
		tag, e := s.DB.Exec(ctx, fmt.Sprintf("delete from %s where %s", name, where), args...)
		if e != nil {
			s.dbError(w, e)
			return
		}
		if tag.RowsAffected() == 0 {
			fail(w, 404, "nao_encontrado")
			return
		}
		send(w, 200, map[string]bool{"ok": true})
	default:
		if r.Method == "PATCH" && id == "" {
			fail(w, 400, "id_obrigatorio")
			return
		}
		var b map[string]json.RawMessage
		if !body(w, r, &b) {
			return
		}
		if len(b) == 0 {
			fail(w, 400, "corpo_vazio")
			return
		}
		keys := []string{}
		for k := range b {
			if !contains(spec.fields, k) {
				fail(w, 400, "campo_nao_permitido:"+k)
				return
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range []string{"photo_path", "avatar_path"} {
			if raw, ok := b[k]; ok && string(raw) != "null" {
				var path string
				if json.Unmarshal(raw, &path) != nil || !ownsPath(path, user(r)) {
					fail(w, 403, "foto_de_outro_usuario")
					return
				}
				var exists bool
				if s.DB.QueryRow(ctx, "select exists(select 1 from stored_files where path=$1 and user_id=$2)", path, user(r)).Scan(&exists) != nil || !exists {
					fail(w, 400, "foto_invalida")
					return
				}
			}
		}
		if raw, ok := b["timezone"]; ok && string(raw) != "null" {
			var tz string
			if json.Unmarshal(raw, &tz) != nil {
				fail(w, 400, "timezone_invalida")
				return
			}
			if _, e := time.LoadLocation(tz); e != nil {
				fail(w, 400, "timezone_invalida")
				return
			}
		}
		if raw, ok := b["token"]; ok {
			var t string
			_ = json.Unmarshal(raw, &t)
			if !(strings.HasPrefix(t, "ExponentPushToken[") || strings.HasPrefix(t, "ExpoPushToken[")) || len(t) > 200 {
				fail(w, 400, "push_token_invalido")
				return
			}
		}
		if name == "profiles" {
			for _, k := range []string{"accepted_terms_at", "accepted_tips_at", "revoked_terms_at", "last_seen_at"} {
				if raw, ok := b[k]; ok && string(raw) != "null" {
					b[k], _ = json.Marshal(time.Now().UTC())
				}
			}
		}
		encoded, _ := json.Marshal(b)
		args = append(args, string(encoded))
		payload := fmt.Sprintf("jsonb_populate_record(null::%s,$%d::jsonb)", name, len(args))
		values := []string{}
		for _, k := range keys {
			values = append(values, "p."+k)
		}
		if r.Method == "POST" {
			if id != "" {
				fail(w, 400, "id_nao_permitido")
				return
			}
			cols := append([]string{spec.owner}, keys...)
			values = append([]string{"$1"}, values...)
			conflict := ""
			if name == "push_tokens" {
				conflict = " where true on conflict(token) do update set platform=excluded.platform,updated_at=now() where push_tokens.user_id=excluded.user_id"
			}
			q = fmt.Sprintf("insert into %s (%s) select %s from %s p%s returning to_jsonb(%s.*)", name, strings.Join(cols, ","), strings.Join(values, ","), payload, conflict, name)
		} else {
			sets := []string{}
			for _, k := range keys {
				sets = append(sets, k+"=p."+k)
			}
			if name == "plants" {
				sets = append(sets, "updated_at=now()")
			}
			patchWhere := fmt.Sprintf("%s.%s=$1 and %s.%s=$2", name, spec.owner, name, spec.key)
			q = fmt.Sprintf("update %s set %s from %s p where %s returning to_jsonb(%s.*)", name, strings.Join(sets, ","), payload, patchWhere, name)
		}
		result, e := rowJSON(ctx, s.DB, q, args...)
		if e == pgx.ErrNoRows && name == "push_tokens" && r.Method == "POST" {
			fail(w, 409, "push_token_em_uso")
			return
		}
		if e == pgx.ErrNoRows {
			fail(w, 404, "nao_encontrado")
			return
		}
		if e != nil {
			s.dbError(w, e)
			return
		}
		status := 200
		if r.Method == "POST" {
			status = 201
		}
		send(w, status, result)
	}
}
