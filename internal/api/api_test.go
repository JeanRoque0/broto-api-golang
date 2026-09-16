package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPaths(t *testing.T) {
	for _, p := range []string{"user/../other/a.jpg", "other/a.jpg", "user//a.jpg", "user/", "user/a\\b.jpg", "user/%2e%2e/x", "/user/a"} {
		if ownsPath(p, "user") {
			t.Fatalf("accepted %s", p)
		}
	}
	if !ownsPath("user/nested/a.jpg", "user") {
		t.Fatal("rejected owned path")
	}
}
func TestNormalize(t *testing.T) {
	if v := normalizeTerm("  LÍRIO-do   campo! "); v != "lirio do campo" {
		t.Fatal(v)
	}
}
func TestReminderCopy(t *testing.T) {
	if v := reminderBody("en-US", []string{"water", "mist"}, 1); v != "1 day late: Water and Mist." {
		t.Fatal(v)
	}
	if v := reminderBody("pt-BR", []string{"prune", "water"}, 0); v != "Hoje: Regar e Podar." {
		t.Fatal(v)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mockReply(text string) *http.Response {
	b, _ := json.Marshal(map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 10, "output_tokens": 10}})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{}}
}
func TestIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	name := "broto_test_" + randomToken()[:12]
	if _, e = admin.Exec(ctx, "create database "+name); e != nil {
		t.Fatal(e)
	}
	defer admin.Exec(ctx, "drop database "+name+" with (force)")
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	u.Path = "/" + name
	c := Config{DatabaseURL: u.String(), StorageDir: t.TempDir(), SigningKey: strings.Repeat("x", 32), DevAuth: true, StorageQuota: 1024, PublicURL: "http://localhost:8080", Origins: "http://localhost:3000", AnthropicKey: "test", VisionModel: "test-vision", FactsModel: "test-facts", ChatModel: "test-chat", SearchModel: "test-search"}
	s, e := New(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	if e = s.migrate(ctx); e != nil {
		t.Fatal("migration replay", e)
	}
	h := s.Routes()
	call := func(method, path, token string, b any, want int) json.RawMessage {
		t.Helper()
		var reader io.Reader
		if raw, ok := b.([]byte); ok {
			reader = bytes.NewReader(raw)
		} else if b != nil {
			data, _ := json.Marshal(b)
			reader = bytes.NewReader(data)
		}
		r := httptest.NewRequest(method, path, reader)
		r.RemoteAddr = "127.0.0.1:1234"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: status %d expected %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w.Body.Bytes()
	}
	decode := func(b json.RawMessage) map[string]any {
		t.Helper()
		var v map[string]any
		if e = json.Unmarshal(b, &v); e != nil {
			t.Fatal(e, string(b))
		}
		return v
	}
	signup := func(email string) (string, string) {
		v := decode(call("POST", "/v1/auth/signup", "", map[string]string{"email": email, "password": "Test-password-123", "name": "Jardim"}, 200))
		return v["access_token"].(string), v["user"].(map[string]any)["id"].(string)
	}
	a, uid := signup("a@example.com")
	b, other := signup("b@example.com")
	call("GET", "/healthz", "", nil, 200)
	call("GET", "/v1/data/plants", "", nil, 401)
	call("GET", "/v1/auth/user", a, nil, 200)
	call("POST", "/v1/auth/login", "", map[string]string{"email": "a@example.com", "password": "wrong-password"}, 401)
	call("PATCH", "/v1/data/profiles", a, map[string]any{"plan": "pro"}, 400)
	call("PATCH", "/v1/data/profiles", a, map[string]any{"display_name": "Novo nome", "timezone": "America/Recife"}, 200)
	call("PATCH", "/v1/data/profiles", a, map[string]any{"timezone": "Nonsense/Nowhere"}, 400)
	group := decode(call("POST", "/v1/data/plant_groups", a, map[string]string{"name": "Sala"}, 201))["id"].(string)
	plant := decode(call("POST", "/v1/data/plants", a, map[string]any{"nickname": "Jiboia", "group_id": group, "watering_interval_days": 7}, 201))["id"].(string)
	call("POST", "/v1/data/plants", b, map[string]any{"nickname": "Invader", "group_id": group}, 400)
	call("GET", "/v1/data/plants/"+plant, b, nil, 404)
	call("PATCH", "/v1/data/plants/"+plant, b, map[string]string{"nickname": "Invader"}, 404)
	call("PATCH", "/v1/data/plants/"+plant, a, map[string]string{"nickname": "Jiboia da sala"}, 200)
	var tasks []map[string]any
	if e = json.Unmarshal(call("GET", "/v1/data/plant_tasks?plant_id="+plant, a, nil, 200), &tasks); e != nil || len(tasks) != 6 {
		t.Fatalf("seed tasks: %v %v", tasks, e)
	}
	call("PATCH", "/v1/data/plant_tasks/"+tasks[0]["id"].(string), a, map[string]any{"interval_days": 14}, 200)
	call("POST", "/v1/data/care_events", b, map[string]any{"plant_id": plant, "kind": "water"}, 400)
	call("POST", "/v1/data/care_events", a, map[string]any{"plant_id": plant, "kind": "water"}, 201)
	call("POST", "/v1/data/chat_messages", a, map[string]string{"role": "assistant"}, 405)
	call("GET", "/v1/data/plants?evil=1", a, nil, 400)
	image := append([]byte{0xff, 0xd8, 0xff}, bytes.Repeat([]byte{0}, 128)...)
	photo := decode(call("POST", "/v1/photos", a, image, 201))["path"].(string)
	call("POST", "/v1/photos/sign", b, map[string]string{"path": photo}, 403)
	signed := decode(call("POST", "/v1/photos/sign", a, map[string]string{"path": photo}, 200))["signedUrl"].(string)
	su, _ := url.Parse(signed)
	call("GET", su.RequestURI(), "", nil, 200)
	call("GET", su.RequestURI()+"x", "", nil, 403)
	call("PATCH", "/v1/data/plants/"+plant, a, map[string]string{"photo_path": photo}, 200)
	call("DELETE", "/v1/photos", a, map[string]string{"path": photo}, 409)
	// Nullable subscription dates never grant free access to paid chat.
	call("POST", "/v1/chat", a, map[string]string{"message": "Como regar?"}, 402)
	if _, e = s.DB.Exec(ctx, "update profiles set plan='pro',plan_expires_at=now()+interval '1 month' where id=$1", uid); e != nil {
		t.Fatal(e)
	}
	s.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		return mockReply("Regue quando a terra estiver seca."), nil
	})}
	chat := decode(call("POST", "/functions/v1/chat", a, map[string]any{"message": "Como regar?", "plantId": plant}, 200))
	thread := chat["threadId"].(string)
	call("POST", "/v1/chat", b, map[string]any{"message": "Oi", "threadId": thread}, 404)
	var messages []any
	_ = json.Unmarshal(call("GET", "/v1/data/chat_messages?thread_id="+thread, a, nil, 200), &messages)
	if len(messages) != 2 {
		t.Fatalf("history %v", messages)
	}
	// A model failure leaves both allowance and history intact.
	s.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("provider unavailable") })}
	call("POST", "/v1/chat", a, map[string]any{"message": "Outra mensagem", "threadId": thread}, 502)
	var used int
	if e = s.DB.QueryRow(ctx, "select chat_month from profiles where id=$1", uid).Scan(&used); e != nil || used != 1 {
		t.Fatalf("chat failed refund: %d %v", used, e)
	}
	s.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		return mockReply(`{"legivel":false,"especie":null,"saude":"saudavel","diagnostico":[]}`), nil
	})}
	out := decode(call("POST", "/v1/identify", a, map[string]any{"photoPaths": []string{photo}}, 200))
	if out["erro"] != "foto_ilegivel" {
		t.Fatal(out)
	}
	if e = s.DB.QueryRow(ctx, "select analyses_month from profiles where id=$1", uid).Scan(&used); e != nil || used != 0 {
		t.Fatalf("unreadable refund %d %v", used, e)
	}
	calls := 0
	s.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return mockReply(`{"legivel":true,"especie":null,"saude":"saudavel","diagnostico":[]}`), nil
	})}
	identified := decode(call("POST", "/functions/v1/identify", a, map[string]any{"photoPaths": []string{photo}, "plantId": plant}, 200))
	if identified["identification_id"] == nil || calls != 1 {
		t.Fatal(identified, calls)
	}
	call("PATCH", "/v1/data/identifications/"+identified["identification_id"].(string), a, map[string]any{"was_helpful": true}, 200)
	call("PATCH", "/v1/data/identifications/"+identified["identification_id"].(string), a, map[string]any{"result": map[string]any{}}, 400)
	call("POST", "/v1/identify", b, map[string]any{"photoPaths": []string{photo}}, 403)
	call("POST", "/v1/photos", a, append([]byte{0xff, 0xd8, 0xff}, make([]byte, 1024)...), 507)
	// Last monthly slot is consumed once under concurrent transactions.
	if _, e = s.DB.Exec(ctx, "update profiles set analyses_month=39 where id=$1", uid); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := s.DB.Begin(ctx)
			if err != nil {
				results <- false
				return
			}
			defer rollback(tx)
			v, err := consume(ctx, tx, uid, false)
			if err == nil {
				err = tx.Commit(ctx)
			}
			results <- err == nil && v.OK
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for won := range results {
		if won {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent credits: %d", wins)
	}
	// Refresh is single-use; logout immediately revokes the replacement.
	fresh := decode(call("POST", "/v1/auth/refresh", a, nil, 200))["access_token"].(string)
	call("GET", "/v1/auth/user", a, nil, 401)
	call("POST", "/v1/auth/logout", fresh, nil, 200)
	call("GET", "/v1/auth/user", fresh, nil, 401)
	// Verification token is consumed atomically and can't be replayed.
	verify := randomToken()
	if _, e = s.DB.Exec(ctx, "insert into auth_tokens(token_hash,user_id,kind,expires_at) values($1,$2,'recovery',now()+interval '1 hour')", hash(verify), uid); e != nil {
		t.Fatal(e)
	}
	recovered := decode(call("POST", "/v1/auth/verify", "", map[string]string{"token_hash": verify, "type": "recovery"}, 200))["access_token"].(string)
	call("POST", "/v1/auth/verify", "", map[string]string{"token_hash": verify, "type": "recovery"}, 400)
	next := decode(call("PATCH", "/v1/auth/password", recovered, map[string]string{"password": "New-password-123"}, 200))["access_token"].(string)
	call("GET", "/v1/auth/user", recovered, nil, 401)
	call("DELETE", "/v1/account", next, nil, 200)
	if _, e = os.Stat(s.filePath(photo)); !os.IsNotExist(e) {
		t.Fatal("account photo not deleted", e)
	}
	call("GET", "/v1/auth/user", next, nil, 401)
	var count int
	if e = s.DB.QueryRow(ctx, "select count(*) from users where id=$1", other).Scan(&count); e != nil || count != 1 {
		t.Fatal("other account affected", e)
	}
	// Search preserves result shape, filters SVG images, and caches normalized terms.
	searches := 0
	s.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.anthropic.com" {
			searches++
			return mockReply(`{"especies":[{"cientifico":"Epipremnum aureum","popular":"Jiboia"}]}`), nil
		}
		payload := `{"items":[]}`
		if strings.Contains(r.URL.Path, "summary") {
			payload = `{"type":"standard","extract":"Planta ornamental.","titles":{"canonical":"Epipremnum aureum"},"thumbnail":{"source":"https://upload.wikimedia.org/test.jpg"}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload)), Header: http.Header{}}, nil
	})}
	search := decode(call("POST", "/v1/search", b, map[string]string{"term": "Jibóia"}, 200))
	if search["fonte"] != "modelo" {
		t.Fatal(search)
	}
	search = decode(call("POST", "/v1/search", b, map[string]string{"term": "jiboia"}, 200))
	if search["fonte"] != "cache" || searches != 1 {
		t.Fatal(search, searches)
	}
	// Scheduled reminders only record provider-accepted deliveries and deduplicate the hour.
	bp := decode(call("POST", "/v1/data/plants", b, map[string]string{"nickname": "Samambaia"}, 201))["id"].(string)
	call("POST", "/v1/data/push_tokens", b, map[string]string{"token": "ExponentPushToken[test]", "platform": "android"}, 201)
	if _, e = s.DB.Exec(ctx, "update profiles set timezone='UTC' where id=$1", other); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, "update plant_tasks set enabled=(kind='water'),next_at=current_date,remind_at=(now() at time zone 'UTC')::time where plant_id=$1", bp); e != nil {
		t.Fatal(e)
	}
	pushes := 0
	s.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		pushes++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"status":"ok","id":"ticket"}]}`)), Header: http.Header{}}, nil
	})}
	s.runJobs(ctx)
	s.runJobs(ctx)
	if pushes != 1 {
		t.Fatalf("reminder pushes: %d", pushes)
	}
	if e = s.DB.QueryRow(ctx, "select count(*) from reminder_events where user_id=$1", other).Scan(&count); e != nil || count != 1 {
		t.Fatalf("reminder history %d %v", count, e)
	}
	orphan := decode(call("POST", "/v1/photos", b, image, 201))["path"].(string)
	kept := decode(call("POST", "/v1/photos", b, image, 201))["path"].(string)
	call("PATCH", "/v1/data/plants/"+bp, b, map[string]string{"photo_path": kept}, 200)
	if _, e = s.DB.Exec(ctx, "insert into identifications(user_id,kind,photo_path,result,created_at) values($1,'species',$2,'{}',now()-interval '8 days')", other, orphan); e != nil {
		t.Fatal(e)
	}

	// Cleanup keeps referenced photos and removes loose analyses, orphan files and old messages.
	if _, e = s.DB.Exec(ctx, "update stored_files set created_at=now()-interval '8 days'"); e != nil {
		t.Fatal(e)
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.cleanup(ctx, tx); e != nil {
		rollback(tx)
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = s.drainFiles(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(s.filePath(orphan)); !os.IsNotExist(e) {
		t.Fatal("orphan was not deleted", e)
	}
	if _, e = os.Stat(s.filePath(kept)); e != nil {
		t.Fatal("referenced photo removed", e)
	}

}
