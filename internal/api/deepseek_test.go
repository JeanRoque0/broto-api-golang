package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDeepSeekRequestAndAccounting(t *testing.T) {
	s := &Server{C: Config{DeepSeekKey: "test-only"}}
	schema := `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`
	s.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.deepseek.com/chat/completions" || r.Header.Get("Authorization") != "Bearer test-only" || r.Header.Get("x-api-key") != "" {
			t.Fatal("incorrect DeepSeek authentication or endpoint")
		}
		var p map[string]any
		if e := json.NewDecoder(r.Body).Decode(&p); e != nil {
			t.Fatal(e)
		}
		if p["model"] != "deepseek-flash" || p["max_tokens"] != float64(2048) || p["thinking"].(map[string]any)["type"] != "disabled" || p["response_format"].(map[string]any)["type"] != "json_object" {
			t.Fatal("invalid DeepSeek request")
		}
		msgs := p["messages"].([]any)
		if len(msgs) != 2 || !strings.Contains(msgs[0].(map[string]any)["content"].(string), schema) {
			t.Fatal("missing system schema")
		}
		parts := msgs[1].(map[string]any)["content"].([]any)
		if parts[0].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/jpeg;base64,/9j/" {
			t.Fatal("image was not forwarded")
		}
		return googleReply(200, map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": `{"ok":true}`, "reasoning_content": "not user-visible"}}}, "usage": map[string]int{"prompt_tokens": 1000, "prompt_cache_hit_tokens": 100, "completion_tokens": 200}}), nil
	})}
	text, cost, e := s.ask(context.Background(), "deepseek-flash", "system", schema, []message{{Role: "user", Content: []any{map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/jpeg;base64,/9j/"}}}}}, 2048)
	if e != nil || text != `{"ok":true}` || cost != 511 {
		t.Fatalf("text=%s cost=%d error=%v", text, cost, e)
	}
}
func TestDeepSeekRejectsInvalidResponses(t *testing.T) {
	schema := `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"upstream error", "secret-do-not-log", 429},
		{"malformed", "invalid", 200},
		{"truncated", `{"choices":[{"finish_reason":"length","message":{"content":"{}"}}]}`, 200},
		{"empty", `{"choices":[{"finish_reason":"stop","message":{"content":" "}}]}`, 200},
		{"wrong schema", `{"choices":[{"finish_reason":"stop","message":{"content":"{\"ok\":\"yes\"}"}}]}`, 200},
		{"null object", `{"choices":[{"finish_reason":"stop","message":{"content":"null"}}]}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{C: Config{DeepSeekKey: "test-only"}, HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}}
			_, _, e := s.ask(context.Background(), "deepseek-flash", "system", schema, []message{{"user", "test"}}, 100)
			if e == nil || strings.Contains(e.Error(), "secret-do-not-log") {
				t.Fatal("invalid response accepted or logged", e)
			}
		})
	}
}
func TestDeepSeekChatDoesNotRequireJSON(t *testing.T) {
	s := &Server{C: Config{DeepSeekKey: "test-only"}}
	s.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		if _, ok := p["response_format"]; ok {
			t.Fatal("chat forced into JSON")
		}
		return mockReply("Resposta normal."), nil
	})}
	text, _, e := s.ask(context.Background(), "deepseek-flash", "system", "", []message{{"user", "test"}}, 100)
	if e != nil || text != "Resposta normal." {
		t.Fatal(e, text)
	}
}

func TestDeepSeekEnvironmentNames(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("SIGNING_KEY", strings.Repeat("x", 32))
	t.Setenv("DEV_AUTO_CONFIRM", "true")
	t.Setenv("DEEPSEEK_API_KEY", "test-only")
	t.Setenv("VISION_MODEL", "vision-selected")
	t.Setenv("TEXT_MODEL", "text-selected")
	t.Setenv("CHAT_MODEL", "chat-selected")
	t.Setenv("SEARCH_MODEL", "search-selected")
	c, e := LoadConfig()
	if e != nil || c.DeepSeekKey != "test-only" || c.VisionModel != "vision-selected" || c.FactsModel != "text-selected" || c.ChatModel != "chat-selected" || c.SearchModel != "search-selected" {
		t.Fatal("DeepSeek env mapping failed", e)
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "legacy-key-must-be-ignored")
	c, e = LoadConfig()
	if e != nil || c.DeepSeekKey != "" {
		t.Fatal("legacy provider enabled implicitly", e)
	}
}
