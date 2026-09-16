package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthEmailMinimumInterval(t *testing.T) {
	f := newFrontendHarness(t)
	m := newLocalSMTP(t, "")
	configureLocalSMTP(f, m)
	f.s.C.SMTPMinIntervalSeconds = 60
	id, _ := f.account()
	f.exec("update users set email='interval@example.invalid' where id=$1", id)
	f.call("POST", "/v1/auth/recover", "", map[string]string{"email": "interval@example.invalid"}, 200)
	r := httptest.NewRequest("POST", "/v1/auth/resend", strings.NewReader(`{"email":"interval@example.invalid"}`))
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" || object(t, w.Body.Bytes())["erro"] != "intervalo_email" {
		t.Fatal("SMTP interval not enforced", w.Code)
	}
	messages, _, _ := m.snapshot()
	if len(messages) != 1 {
		t.Fatal("rate limited email was sent")
	}
	f.exec("update auth_email_sends set attempted_at=now()-interval '61 seconds' where user_id=$1", id)
	f.call("POST", "/v1/auth/resend", "", map[string]string{"email": "interval@example.invalid"}, 200)
	messages, _, _ = m.snapshot()
	if len(messages) != 2 {
		t.Fatal("SMTP interval did not expire")
	}
	other, _ := f.account()
	start := make(chan struct{})
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			<-start
			results <- f.s.authEmail(context.Background(), other, "other@example.invalid", "email")
		}()
	}
	close(start)
	sent := 0
	for i := 0; i < 8; i++ {
		e := <-results
		if e == nil {
			sent++
		} else if !errors.Is(e, errEmailInterval) {
			t.Error(e)
		}
	}
	if sent != 1 {
		t.Fatal("concurrent sends bypassed per-user interval", sent)
	}
	messages, _, _ = m.snapshot()
	if len(messages) != 3 {
		t.Fatal("wrong SMTP delivery count")
	}
	var tokens int
	if e := f.s.DB.QueryRow(context.Background(), "select count(*) from auth_tokens").Scan(&tokens); e != nil || tokens != 3 {
		t.Fatal("throttled sends generated tokens", tokens, e)
	}
}

func TestAnthropicEffortPreservesStructuredOutput(t *testing.T) {
	for _, tc := range []struct {
		model, effort, schema string
		wantEffort            bool
	}{
		{"claude-opus-5", "medium", `{"type":"object","properties":{}}`, true},
		{"claude-opus-5", "medium", "", true},
		{"claude-sonnet-5", "low", "", true},
		{"claude-haiku-4-5", "medium", `{"type":"object","properties":{}}`, false},
		{"claude-opus-5", "", "", false},
	} {
		t.Run(tc.model+tc.effort+tc.schema, func(t *testing.T) {
			s := &Server{C: Config{AnthropicKey: "test-only", AnthropicEffort: tc.effort}}
			s.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				var payload map[string]any
				if e := json.NewDecoder(r.Body).Decode(&payload); e != nil {
					t.Fatal(e)
				}
				output, _ := payload["output_config"].(map[string]any)
				if (output["effort"] != nil) != tc.wantEffort {
					t.Fatal("wrong effort application")
				}
				if tc.wantEffort && output["effort"] != tc.effort {
					t.Fatal("effort not preserved")
				}
				if (output["format"] != nil) != (tc.schema != "") {
					t.Fatal("structured output overwritten")
				}
				if payload["max_tokens"] != float64(2048) || payload["model"] != tc.model || r.Header.Get("anthropic-version") != "2023-06-01" {
					t.Fatal("Anthropic request contract changed")
				}
				return googleReply(200, map[string]any{"stop_reason": "end_turn", "content": []any{map[string]string{"type": "thinking", "thinking": ""}, map[string]string{"type": "text", "text": "Resposta de teste"}}}), nil
			})}
			reply, _, e := s.ask(context.Background(), tc.model, "Test", tc.schema, []message{{Role: "user", Content: "test"}}, 2048)
			if e != nil || reply != "Resposta de teste" {
				t.Fatal("thinking block interfered with visible answer", e)
			}
		})
	}
}
