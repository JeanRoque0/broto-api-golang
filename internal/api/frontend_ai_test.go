package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const frontendVision = `{"legivel":true,"especie":{"comum":"Jiboia","cientifico":"Epipremnum aureum","confianca":0.95},"saude":"saudavel","diagnostico":[]}`
const frontendFacts = `{"cuidados":{"rega_dias":7,"luz":"luz_indireta","luz_nota":"Use luz indireta.","adubo":"mensal","adubo_nota":"Siga o rótulo.","vaporizar_dias":null,"girar_dias":14,"replantar_meses":12,"podar_mes":null},"toxica_para_pets":true,"temperatura":{"min_c":18,"max_c":28,"nota":"Evite frio."},"cultivo":"Estaquia.","simbolismo":""}`

func TestFrontendAnalysisToPlantAndChat(t *testing.T) {
	f := newFrontendHarness(t)
	id, token := f.account()
	_, other := f.account()
	photo := f.photo(token)
	vision, facts, chat := 0, 0, 0
	f.s.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Model    string    `json:"model"`
			Messages []message `json:"messages"`
		}
		if e := json.NewDecoder(r.Body).Decode(&payload); e != nil {
			return nil, e
		}
		switch payload.Model {
		case "vision-test":
			vision++
			if len(payload.Messages) != 2 {
				return nil, errors.New("vision request must contain one user message")
			}
			parts, ok := payload.Messages[1].Content.([]any)
			if !ok || len(parts) != 2 {
				return nil, errors.New("vision requires one image and text")
			}
			return mockReply(frontendVision), nil
		case "facts-test":
			facts++
			return mockReply(frontendFacts), nil
		case "chat-test":
			chat++
			want := 1
			if chat == 2 {
				want = 3
			}
			if len(payload.Messages) != want+1 {
				return nil, errors.New("chat history window lost user or assistant message")
			}
			return mockReply("Cuide da rega observando a terra."), nil
		default:
			return nil, errors.New("unexpected external model")
		}
	})}
	var first map[string]any
	for _, language := range []string{"pt-BR", "pt-BR", "en-US"} {
		result := object(t, f.call("POST", "/functions/v1/identify", token, map[string]any{"photoPaths": []string{photo}, "language": language}, 200))
		for _, key := range []string{"especie", "cuidados", "toxica_para_pets", "temperatura", "cultivo", "simbolismo", "saude", "diagnostico", "como_confirmar", "identification_id"} {
			if _, ok := result[key]; !ok {
				t.Fatal("AnalysisResponse missing", key)
			}
		}
		if result["saude"] != "saudavel" || result["identification_id"] == nil {
			t.Fatal(result)
		}
		if result["cuidados"].(map[string]any)["rega_dias"] != float64(7) {
			t.Fatal(result)
		}
		if first == nil {
			first = result
		}
	}
	if vision != 3 || facts != 2 {
		t.Fatalf("expected one vision per analysis and cache per language: vision=%d facts=%d", vision, facts)
	}
	p := f.plant(token, map[string]any{"photo_path": photo, "watering_interval_days": 7, "species_scientific": "Epipremnum aureum", "species_common": "Jiboia", "toxic_to_pets": true})
	ident := first["identification_id"].(string)
	f.call("PATCH", "/v1/data/identifications/"+ident, token, map[string]any{"plant_id": p, "was_helpful": true}, 200)
	resolved := object(t, f.call("PATCH", "/v1/data/identifications/"+ident, token, map[string]any{"resolved_at": time.Now().UTC().Format(time.RFC3339Nano)}, 200))
	if resolved["resolved_at"] == nil {
		t.Fatal(resolved)
	}
	resolved = object(t, f.call("PATCH", "/v1/data/identifications/"+ident, token, map[string]any{"resolved_at": nil}, 200))
	if resolved["resolved_at"] != nil {
		t.Fatal(resolved)
	}
	f.call("PATCH", "/v1/data/identifications/"+ident, other, map[string]any{"was_helpful": false}, 404)
	f.call("PATCH", "/v1/data/identifications/"+ident, token, map[string]any{"result": map[string]any{}}, 400)
	cached := object(t, f.call("GET", "/v1/species-facts?scientific="+url.QueryEscape("Epipremnum aureum")+"&language=pt-BR", token, nil, 200))
	if cached["toxica_para_pets"] != true {
		t.Fatal(cached)
	}
	f.exec("update profiles set chat_expires_at=now()+interval '1 month' where id=$1", id)
	one := object(t, f.call("POST", "/functions/v1/chat", token, map[string]any{"message": "Quando regar?", "plantId": p}, 200))
	thread := one["threadId"].(string)
	two := object(t, f.call("POST", "/functions/v1/chat", token, map[string]any{"message": "E a luz?", "threadId": thread}, 200))
	if two["restantes"] != float64(148) {
		t.Fatal(two)
	}
	history := records(t, f.call("GET", "/v1/data/chat_messages?thread_id="+thread+"&order=asc&limit=200", token, nil, 200))
	if len(history) != 4 {
		t.Fatal(history)
	}
	for i, row := range history {
		assertFrontendRecord(t, "ChatMessage", row)
		want := "user"
		if i%2 == 1 {
			want = "assistant"
		}
		if row["role"] != want {
			t.Fatal("message chronology", history)
		}
	}
	threads := records(t, f.call("GET", "/v1/data/chat_threads?limit=50", token, nil, 200))
	assertFrontendRecord(t, "ChatThread", threads[0])
	if rows := records(t, f.call("GET", "/v1/data/chat_messages?thread_id="+thread, other, nil, 200)); len(rows) != 0 {
		t.Fatal(rows)
	}
	f.call("DELETE", "/v1/data/chat_threads/"+thread, token, nil, 200)
	if rows := records(t, f.call("GET", "/v1/data/chat_messages?thread_id="+thread, token, nil, 200)); len(rows) != 0 {
		t.Fatal("thread delete did not cascade")
	}
}
func TestFrontendAnalysisFailureAndCancellation(t *testing.T) {
	f := newFrontendHarness(t)
	id, token := f.account()
	photo := f.photo(token)
	for _, tc := range []struct {
		name, text string
		status     int
		code       string
	}{
		{"illegible", `{"legivel":false,"especie":null,"saude":"saudavel","diagnostico":[]}`, 200, "foto_ilegivel"},
		{"missing required fields", `{"especie":null}`, 502, "falha_analise"},
		{"malformed JSON", `{"broken":`, 502, "falha_analise"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := *f
			local.t = t
			f.s.HTTP = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return mockReply(tc.text), nil })}
			b := object(t, local.call("POST", "/v1/identify", token, map[string]any{"photoPaths": []string{photo}}, tc.status))
			if b["erro"] != tc.code {
				t.Fatal(b)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.s.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) { cancel(); return nil, r.Context().Err() })}
	r := httptest.NewRequest("POST", "/v1/identify", strings.NewReader(`{"photoPaths":["`+photo+`"]}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 502 {
		t.Fatal(w.Code, w.Body.String())
	}
	p := object(t, f.call("GET", "/v1/data/profiles", token, nil, 200))
	if p["free_used"] != float64(0) || p["analyses_month"] != float64(0) || p["analyses_today"] != float64(0) || p["welcome_credits"] != float64(2) {
		t.Fatalf("failure consumed credits: %v", p)
	}
	var saved int
	if e := f.s.DB.QueryRow(context.Background(), "select count(*) from identifications where user_id=$1", id).Scan(&saved); e != nil || saved != 0 {
		t.Fatal(saved, e)
	}
}
func TestFrontendArchivedReminderAndProfileTime(t *testing.T) {
	f := newFrontendHarness(t)
	id, token := f.account()
	p := f.plant(token, nil)
	// With no task override, the profile's hour controls eligibility.
	f.exec("update profiles set timezone='UTC',reminder_time=(now() at time zone 'UTC')::time where id=$1", id)
	f.exec("update plant_tasks set enabled=(kind='water'),next_at=current_date,remind_at=null where plant_id=$1", p)
	count := func() int {
		t.Helper()
		var n int
		if e := f.s.DB.QueryRow(context.Background(), "select count(*) from due_reminders() where user_id=$1", id).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	if n := count(); n != 1 {
		t.Fatal("profile hour ignored", n)
	}
	f.exec("update profiles set reminder_time=((now() at time zone 'UTC')+interval '2 hours')::time where id=$1", id)
	if n := count(); n != 0 {
		t.Fatal("wrong hour", n)
	}
	f.exec("update plant_tasks set remind_at=(now() at time zone 'UTC')::time where plant_id=$1", p)
	if n := count(); n != 1 {
		t.Fatal("task override ignored", n)
	}
	f.call("PATCH", "/v1/data/plants/"+p, token, map[string]any{"archived_at": time.Now().UTC().Format(time.RFC3339Nano)}, 200)
	if n := count(); n != 0 {
		t.Fatal("archived plant notified", n)
	}
}
