package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var responseSchemas sync.Map

// DeepSeek JSON mode guarantees JSON syntax, not the application's schema.
// Validate locally before persisting data or committing the credit transaction.
func validateModelJSON(schema, text string) error {
	var value any
	if json.Unmarshal([]byte(text), &value) != nil {
		return errors.New("model returned invalid JSON")
	}
	cached, ok := responseSchemas.Load(schema)
	if !ok {
		var document any
		if json.Unmarshal([]byte(schema), &document) != nil {
			return errors.New("invalid internal AI schema")
		}
		compiler := jsonschema.NewCompiler()
		if e := compiler.AddResource("response.json", document); e != nil {
			return errors.New("invalid internal AI schema")
		}
		compiled, e := compiler.Compile("response.json")
		if e != nil {
			return errors.New("invalid internal AI schema")
		}
		cached, _ = responseSchemas.LoadOrStore(schema, compiled)
	}
	if cached.(*jsonschema.Schema).Validate(value) != nil {
		return errors.New("model response does not match schema")
	}
	return nil
}

func (s *Server) ask(ctx context.Context, model, system, schema string, messages []message, max int) (string, int, error) {
	if model == "" || s.C.DeepSeekKey == "" {
		return "", 0, errors.New("AI configuration missing")
	}
	if schema != "" {
		system += "\nRetorne somente um objeto JSON, sem markdown, seguindo integralmente este JSON Schema:\n" + schema
	}
	history := make([]message, 0, len(messages)+1)
	history = append(history, message{Role: "system", Content: system})
	history = append(history, messages...)
	payload := map[string]any{"model": model, "messages": history, "max_tokens": max, "stream": false, "thinking": map[string]string{"type": "disabled"}}
	if schema != "" {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	b, e := json.Marshal(payload)
	if e != nil {
		return "", 0, e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.deepseek.com/chat/completions", bytes.NewReader(b))
	if e != nil {
		return "", 0, e
	}
	req.Header.Set("Authorization", "Bearer "+s.C.DeepSeekKey)
	req.Header.Set("Content-Type", "application/json")
	response, e := s.HTTP.Do(req)
	if e != nil {
		return "", 0, errors.New("DeepSeek request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("DeepSeek HTTP status %d", response.StatusCode)
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Input   int `json:"prompt_tokens"`
			Output  int `json:"completion_tokens"`
			Cached  int `json:"prompt_cache_hit_tokens"`
			Details struct {
				Cached int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if e != nil || len(raw) > 2<<20 || json.Unmarshal(raw, &result) != nil {
		return "", 0, errors.New("invalid DeepSeek response")
	}
	if len(result.Choices) != 1 || result.Choices[0].Finish != "stop" {
		return "", 0, errors.New("incomplete DeepSeek response")
	}
	text := strings.TrimSpace(result.Choices[0].Message.Content)
	if text == "" {
		return "", 0, errors.New("empty DeepSeek response")
	}
	if schema != "" {
		if e = validateModelJSON(schema, text); e != nil {
			return "", 0, e
		}
	}
	// Conservative estimate in USD micros, using published peak Flash rates on
	// 2026-09-21. Off-peak billing can be lower; this is not the provider invoice.
	cost := 0
	if model == "deepseek-flash" {
		cached := result.Usage.Cached
		if cached == 0 {
			cached = result.Usage.Details.Cached
		}
		cached = min(maxInt(cached, 0), maxInt(result.Usage.Input, 0))
		cost = int(float64(maxInt(result.Usage.Input, 0)-cached)*.3 + float64(cached)*.006 + float64(maxInt(result.Usage.Output, 0))*1.2 + .5)
	}
	return text, cost, nil
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
