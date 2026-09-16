package api

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Pinned to the inspected frontend commit. Tests need no checkout of broto-app,
// npm install, emulator, Supabase account or paid external service.
func assertFrontendRecord(t *testing.T, name string, value map[string]any) {
	t.Helper()
	b, e := os.ReadFile("testdata/frontend-contract.json")
	if e != nil {
		t.Fatal(e)
	}
	var contract struct {
		Interfaces map[string]map[string]struct {
			Type     string `json:"type"`
			Nullable bool   `json:"nullable"`
		} `json:"interfaces"`
	}
	if e = json.Unmarshal(b, &contract); e != nil {
		t.Fatal(e)
	}
	fields, ok := contract.Interfaces[name]
	if !ok {
		t.Fatal("missing interface", name)
	}
	for k, spec := range fields {
		v, present := value[k]
		if !present {
			t.Errorf("%s.%s missing", name, k)
			continue
		}
		if v == nil {
			if !spec.Nullable {
				t.Errorf("%s.%s unexpectedly null", name, k)
			}
			continue
		}
		valid := false
		switch spec.Type {
		case "string":
			_, valid = v.(string)
		case "number":
			_, valid = v.(float64)
		case "boolean":
			_, valid = v.(bool)
		}
		if !valid {
			t.Errorf("%s.%s type %T want %s", name, k, v, spec.Type)
		}
	}
}
func TestFrontendSessionAndConsent(t *testing.T) {
	f := newFrontendHarness(t)
	session := object(t, f.call("POST", "/v1/auth/signup", "", map[string]string{"email": "Pessoa@example.invalid", "password": "Abcdef#1", "name": "Úrsula"}, 200))
	token := session["access_token"].(string)
	u := session["user"].(map[string]any)
	id := u["id"].(string)
	if u["email"] != "pessoa@example.invalid" || u["user_metadata"] == nil {
		t.Fatalf("profile/chat/export need email + metadata in session: %v", u)
	}
	if u["user_metadata"].(map[string]any)["name"] != "Úrsula" {
		t.Fatal(u)
	}
	for _, secret := range []string{"password_hash", "raw_user_meta_data", "token_hash"} {
		if _, ok := u[secret]; ok {
			t.Fatal("private user field exposed", secret)
		}
	}
	profile := object(t, f.call("GET", "/v1/data/profiles", token, nil, 200))
	assertFrontendRecord(t, "Profile", profile)
	accept := func(version string) map[string]any {
		return object(t, f.call("PATCH", "/v1/data/profiles", token, map[string]any{"accepted_terms_at": time.Now().UTC().Format(time.RFC3339Nano), "terms_version": version, "revoked_terms_at": nil}, 200))
	}
	accepted := accept("v1")
	if accepted["accepted_terms_at"] == nil || accepted["terms_version"] != "v1" {
		t.Fatal(accepted)
	}
	f.call("PATCH", "/v1/data/profiles", token, map[string]any{"revoked_terms_at": time.Now().UTC().Format(time.RFC3339Nano)}, 200)
	accepted = accept("v2")
	if accepted["revoked_terms_at"] != nil || accepted["terms_version"] != "v2" {
		t.Fatal(accepted)
	}
	f.call("PATCH", "/v1/data/profiles", token, map[string]any{"temperature_unit": "fahrenheit", "language": "en-US", "last_seen_at": time.Now().UTC().Format(time.RFC3339Nano), "reminder_time": "08:30", "timezone": "America/Recife"}, 200)
	f.call("PATCH", "/v1/data/profiles", token, map[string]any{"temperature_unit": "kelvin"}, 400)
	f.call("PATCH", "/v1/data/profiles", token, map[string]any{"paid_credits": 999}, 400)
	refreshed := object(t, f.call("POST", "/v1/auth/refresh", token, nil, 200))
	if refreshed["user"].(map[string]any)["email"] != u["email"] {
		t.Fatal(refreshed)
	}
	f.call("GET", "/v1/auth/user", token, nil, 401)
	f.exec("update sessions set expires_at=now()-interval '1 second' where user_id=$1", id)
	f.call("GET", "/v1/auth/user", refreshed["access_token"].(string), nil, 401)
}
func TestFrontendGardenAndRecheck(t *testing.T) {
	f := newFrontendHarness(t)
	_, token := f.account()
	_, other := f.account()
	group := object(t, f.call("POST", "/v1/data/plant_groups", token, map[string]string{"name": "Sala"}, 201))
	assertFrontendRecord(t, "PlantGroup", group)
	p := f.plant(token, map[string]any{"group_id": group["id"], "watering_interval_days": 5, "fertilizer": "mensal", "mist_days": 3, "rotate_days": 14, "repot_months": 12, "prune_month": 9, "temp_min_c": 18, "temp_max_c": 28, "species_scientific": "Epipremnum aureum", "species_common": "Jiboia", "light": "luz_indireta", "toxic_to_pets": true})
	plant := object(t, f.call("GET", "/v1/data/plants/"+p, token, nil, 200))
	assertFrontendRecord(t, "Plant", plant)
	tasks := records(t, f.call("GET", "/v1/data/plant_tasks?plant_id="+p, token, nil, 200))
	if len(tasks) != 6 {
		t.Fatal(tasks)
	}
	var water string
	for _, task := range tasks {
		assertFrontendRecord(t, "PlantTask", task)
		if task["kind"] == "water" {
			water = task["id"].(string)
			if task["interval_days"] != float64(5) {
				t.Fatal(task)
			}
		}
	}
	// logCareEvent + advancePlantTask: the adapter currently needs three writes.
	event := object(t, f.call("POST", "/v1/data/care_events", token, map[string]any{"plant_id": p, "kind": "water", "note": "Terra seca", "happened_at": "2026-09-15T20:30:00-03:00"}, 201))
	assertFrontendRecord(t, "CareEvent", event)
	f.call("PATCH", "/v1/data/plant_tasks/"+water, token, map[string]any{"next_at": "2026-09-20"}, 200)
	f.call("PATCH", "/v1/data/plants/"+p, token, map[string]any{"last_watered_at": "2026-09-15T20:30:00-03:00"}, 200)
	task := object(t, f.call("GET", "/v1/data/plant_tasks/"+water, token, nil, 200))
	if task["next_at"] != "2026-09-20" {
		t.Fatal(task)
	}
	f.call("PATCH", "/v1/data/plant_tasks/"+water, other, map[string]any{"next_at": "2026-09-21"}, 404)
	// scheduleRecheck can adapt .upsert to find/create or patch by owned task ID.
	recheck := object(t, f.call("POST", "/v1/data/plant_tasks", token, map[string]any{"plant_id": p, "kind": "recheck", "interval_days": 7, "next_at": "2026-09-22", "remind_at": "10:30", "enabled": true}, 201))
	f.call("PATCH", "/v1/data/plant_tasks/"+recheck["id"].(string), token, map[string]any{"interval_days": 14, "next_at": "2026-09-29"}, 200)
	f.call("DELETE", "/v1/data/plant_tasks/"+recheck["id"].(string), token, nil, 200)
	// Exact replacement for the app's join filtering out archived plants.
	f.call("PATCH", "/v1/data/plants/"+p, token, map[string]any{"archived_at": time.Now().UTC().Format(time.RFC3339Nano)}, 200)
	if got := records(t, f.call("GET", "/v1/data/plant_tasks?active=true", token, nil, 200)); len(got) != 0 {
		t.Fatalf("archived tasks leaked into agenda: %v", got)
	}
	if got := records(t, f.call("GET", "/v1/data/plants?archived_at=null", token, nil, 200)); len(got) != 0 {
		t.Fatal(got)
	}
	if got := records(t, f.call("GET", "/v1/data/plant_tasks?plant_id="+p, token, nil, 200)); len(got) != 6 {
		t.Fatal("export must retain archived tasks")
	}
	f.call("DELETE", "/v1/data/plant_groups/"+group["id"].(string), token, nil, 200)
	if got := object(t, f.call("GET", "/v1/data/plants/"+p, token, nil, 200)); got["group_id"] != nil {
		t.Fatal(got)
	}
}
func TestFrontendAvatarReplacement(t *testing.T) {
	f := newFrontendHarness(t)
	_, token := f.account()
	_, other := f.account()
	old := f.photo(token)
	f.call("PATCH", "/v1/data/profiles", token, map[string]string{"avatar_path": old}, 200)
	next := f.photo(token)
	f.call("PATCH", "/v1/data/profiles", token, map[string]string{"avatar_path": next}, 200)
	f.call("DELETE", "/v1/photos", token, map[string]string{"path": old}, 200)
	f.call("DELETE", "/v1/photos", token, map[string]string{"path": next}, 409)
	f.call("POST", "/v1/photos/sign", other, map[string]string{"path": next}, 403)
	signed := object(t, f.call("POST", "/v1/photos/sign", token, map[string]string{"path": next}, 200))["signedUrl"].(string)
	u, _ := url.Parse(signed)
	q := u.Query()

	q.Set("expires", "1")
	q.Set("signature", f.s.signature(next, "1"))
	u.RawQuery = q.Encode()
	f.call("GET", u.RequestURI(), "", nil, 403)
	f.call("PATCH", "/v1/data/profiles", token, map[string]any{"avatar_path": nil}, 200)
	f.call("DELETE", "/v1/photos", token, map[string]string{"path": next}, 200)
	u, _ = url.Parse(signed)
	f.call("GET", u.RequestURI(), "", nil, 404)
}
func TestFrontendPushReRegistration(t *testing.T) {
	f := newFrontendHarness(t)
	_, token := f.account()
	_, other := f.account()
	body := map[string]string{"token": "ExponentPushToken[frontend]", "platform": "android"}
	first := object(t, f.call("POST", "/v1/data/push_tokens", token, body, 201))
	body["platform"] = "ios"
	second := object(t, f.call("POST", "/v1/data/push_tokens", token, body, 201))
	if first["token"] != second["token"] || second["platform"] != "ios" {
		t.Fatal(second)
	}
	f.call("POST", "/v1/data/push_tokens", other, body, 409)
	if got := records(t, f.call("GET", "/v1/data/push_tokens", token, nil, 200)); len(got) != 1 {
		t.Fatal(got)
	}
	f.call("DELETE", "/v1/data/push_tokens/"+url.PathEscape(body["token"]), token, nil, 200)
	// Logout must unregister before revoking session; another account can then register.
	f.call("POST", "/v1/auth/logout", token, nil, 200)
	f.call("POST", "/v1/data/push_tokens", other, body, 201)
}
func TestFrontendSpeciesFacts(t *testing.T) {
	f := newFrontendHarness(t)
	_, token := f.account()
	path := "/v1/species-facts?scientific=" + url.QueryEscape("Epipremnum aureum") + "&language=pt-BR"
	f.call("GET", path, "", nil, 401)
	if got := strings.TrimSpace(string(f.call("GET", path, token, nil, 200))); got != "null" {
		t.Fatal(got)
	}
	f.exec("insert into species_facts(scientific,language,data) values('Epipremnum aureum','pt-BR','{\"cuidados\":null,\"toxica_para_pets\":true,\"temperatura\":null,\"cultivo\":\"Estaquia\",\"simbolismo\":\"\"}')")
	got := object(t, f.call("GET", path, token, nil, 200))
	if got["cultivo"] != "Estaquia" {
		t.Fatal(got)
	}
	if got := strings.TrimSpace(string(f.call("GET", strings.Replace(path, "pt-BR", "en-US", 1), token, nil, 200))); got != "null" {
		t.Fatal(got)
	}
	f.call("GET", "/v1/species-facts?language=pt-BR", token, nil, 400)
}
func TestFrontendPaginationRemindersAndAnnouncements(t *testing.T) {
	f := newFrontendHarness(t)
	id, token := f.account()
	oid, other := f.account()
	f.exec("insert into reminder_events(user_id,kind,title,body) select $1,'care','Regar','Hoje' from generate_series(1,65)", id)
	f.exec("insert into reminder_events(user_id,kind,title,body) values($1,'care','Outra conta','Hoje')", oid)
	first := records(t, f.call("GET", "/v1/data/reminder_events?limit=60", token, nil, 200))
	if len(first) != 60 {
		t.Fatal(len(first))
	}
	rest := records(t, f.call("GET", "/v1/data/reminder_events?limit=60&offset=60", token, nil, 200))
	if len(rest) != 5 {
		t.Fatal(len(rest))
	}
	seen := map[any]bool{}
	for _, row := range append(first, rest...) {
		if seen[row["id"]] {
			t.Fatal("duplicate across pages")
		}
		seen[row["id"]] = true
	}
	if got := object(t, f.call("GET", "/v1/reminders/unread-count", token, nil, 200)); got["count"] != float64(65) {
		t.Fatal(got)
	}
	f.call("POST", "/v1/reminders/read", token, nil, 200)
	f.call("POST", "/v1/reminders/read", token, nil, 200)
	if got := object(t, f.call("GET", "/v1/reminders/unread-count", token, nil, 200)); got["count"] != float64(0) {
		t.Fatal(got)
	}
	if got := object(t, f.call("GET", "/v1/reminders/unread-count", other, nil, 200)); got["count"] != float64(1) {
		t.Fatal(got)
	}
	f.exec(`insert into announcements(title,body,starts_at,ends_at) values ('{"pt-BR":"Atual"}','{}',now()-interval '1 day',null),('{"pt-BR":"Futuro"}','{}',now()+interval '1 day',null),('{"pt-BR":"Expirado"}','{}',now()-interval '2 days',now()-interval '1 day')`)
	announcements := records(t, f.call("GET", "/v1/data/announcements?limit=1", token, nil, 200))
	if len(announcements) != 1 || announcements[0]["title"].(map[string]any)["pt-BR"] != "Atual" {
		t.Fatal(announcements)
	}
	f.call("PATCH", "/v1/data/profiles", token, map[string]any{"dismissed_announcement": announcements[0]["id"]}, 200)
	f.call("POST", "/v1/data/announcements", token, map[string]string{"kind": "general"}, 405)
	// Export must traverse all pages; listUserCareEvents asks for 500 records.
	p := f.plant(token, nil)
	f.exec("insert into care_events(user_id,plant_id,kind,happened_at) select $1,$2,'water',now()+make_interval(secs=>g) from generate_series(1,501) g", id, p)
	n := 0
	for offset := 0; offset < 600; offset += 200 {
		rows := records(t, f.call("GET", "/v1/data/care_events?limit=200&offset="+jsonNumber(offset)+"&order=asc", token, nil, 200))
		for _, row := range rows {
			assertFrontendRecord(t, "CareEvent", row)
		}
		n += len(rows)
		if len(rows) < 200 {
			break
		}
	}
	if n != 501 {
		t.Fatal(n)
	}
	f.call("GET", "/v1/data/care_events?limit=500", token, nil, 400)
	f.call("GET", "/v1/data/care_events?order=asc;drop+table+users", token, nil, 400)
}
func jsonNumber(n int) string { b, _ := json.Marshal(n); return string(b) }
