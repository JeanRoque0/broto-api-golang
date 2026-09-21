package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// These opt-in checks deliver mail and spend DeepSeek tokens. The regular
// suite never enables them; scripts/test-live.py requires explicit flags.
func TestLiveSMTP(t *testing.T) {
	to := os.Getenv("BROTO_LIVE_SMTP_TO")
	if to == "" {
		t.Skip("explicit live SMTP recipient required")
	}
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	c, e := LoadConfig()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	e = sendSMTPText(ctx, c, to, "Broto - teste de SMTP", "Teste autorizado da broto-api.\r\nConexao SMTP com STARTTLS e autenticacao.\r\nEsta mensagem nao altera sua conta e nao requer nenhuma acao.\r\n", nil)
	if e != nil {
		message := strings.ReplaceAll(e.Error(), c.SMTPPassword, "[redacted]")
		t.Fatal("SMTP live check failed:", message)
	}
	t.Log("SMTP accepted one test message; inbox delivery must be checked by recipient")
}

func TestLiveDeepSeekChat(t *testing.T) {
	if os.Getenv("BROTO_LIVE_CHAT") != "true" {
		t.Skip("explicit live DeepSeek flag required")
	}
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	c, e := LoadConfig()
	if e != nil {
		t.Fatal(e)
	}
	if c.DeepSeekKey == "" {
		t.Fatal("DEEPSEEK_API_KEY missing")
	}
	f := newFrontendHarness(t)
	f.s.C.DeepSeekKey = c.DeepSeekKey
	f.s.C.ChatModel = c.ChatModel
	f.s.C.ChatMaxTokens = c.ChatMaxTokens
	// Real requests are restricted to DeepSeek; fake user data stays disposable.
	f.s.HTTP = &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.deepseek.com" || r.URL.Path != "/chat/completions" {
			t.Error("unexpected external target")
			return nil, context.Canceled
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	id, token := f.account()
	f.exec("update profiles set plan='pro',plan_expires_at=now()+interval '1 day' where id=$1", id)
	plant := f.plant(token, map[string]any{"nickname": "Jiboia de teste", "species_common": "Jiboia"})
	first := object(t, f.call("POST", "/v1/chat", token, map[string]any{"message": "Como verifico se a minha jiboia precisa de agua? Responda em ate duas frases.", "plantId": plant}, 200))
	thread := first["threadId"].(string)
	if strings.TrimSpace(first["reply"].(string)) == "" {
		t.Fatal("empty first reply")
	}
	second := object(t, f.call("POST", "/v1/chat", token, map[string]any{"message": "Qual planta estamos discutindo? Responda somente com o nome.", "threadId": thread}, 200))
	if second["threadId"] != thread || !strings.Contains(strings.ToLower(second["reply"].(string)), "jiboia") {
		t.Fatal("follow-up did not preserve conversation")
	}
	messages := records(t, f.call("GET", "/v1/data/chat_messages?thread_id="+thread+"&order=asc", token, nil, 200))
	if len(messages) != 4 || messages[0]["role"] != "user" || messages[1]["role"] != "assistant" || messages[2]["role"] != "user" || messages[3]["role"] != "assistant" {
		t.Fatal("chat history not persisted correctly")
	}
	var used int
	if e := f.s.DB.QueryRow(context.Background(), "select chat_today from profiles where id=$1", id).Scan(&used); e != nil || used != 2 {
		t.Fatal("chat credit usage incorrect", used, e)
	}
	t.Logf("Two real chat turns passed: model=%s, 4 messages persisted", c.ChatModel)
}
