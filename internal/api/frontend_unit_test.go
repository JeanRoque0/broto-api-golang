package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Cases mirror src/utils/password.ts and its tests. The additional 72-byte
// ceiling is bcrypt's bound, which the frontend must also display.
func TestFrontendPasswordRules(t *testing.T) {
	for _, tc := range []struct {
		name, password string
		valid          bool
	}{
		{"eight characters", "Abcdef#1", true},
		{"uppercase accent", "Único#2026", true},
		{"space is special", "Exemplo 2026", true},
		{"too short", "Ex#1", false},
		{"missing uppercase", "exemplo#2026", false},
		{"missing special", "Exemplo2026", false},
		{"empty", "", false},
		{"bcrypt bound", "A#" + strings.Repeat("x", 71), false},
		{"unicode byte bound", "Á#" + strings.Repeat("á", 35), false},
		{"javascript counts surrogate pairs", "A#aaaa😀", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validPassword(tc.password); got != tc.valid {
				t.Fatalf("valid=%v want %v", got, tc.valid)
			}
		})
	}
}

// useAnalyzing.ts and useChat.ts route screens using these exact codes.
func TestFrontendCreditErrors(t *testing.T) {
	for _, tc := range []struct {
		reason string
		chat   bool
		status int
		code   string
		cap    int
	}{
		{"no_credits", false, 402, "sem_credito", 0},
		{"month_cap", false, 429, "limite_mensal", 40},
		{"daily_cap", false, 429, "limite_diario", 50},
		{"no_plan", true, 402, "no_plan", 0},
		{"month_cap", true, 429, "month_cap", 150},
		{"daily_cap", true, 429, "daily_cap", 30},
	} {
		t.Run(tc.code, func(t *testing.T) {
			w := httptest.NewRecorder()
			creditError(w, allowance{Reason: tc.reason, Cap: tc.cap}, tc.chat)
			var out map[string]any
			if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
				t.Fatal(e)
			}
			if w.Code != tc.status || out["erro"] != tc.code {
				t.Fatalf("got %d %v", w.Code, out)
			}
			if tc.cap > 0 {
				key := "limite"
				if tc.chat {
					key = "cap"
				}
				if out[key] != float64(tc.cap) {
					t.Fatal(out)
				}
			}
		})
	}
}
func TestFrontendSearchNormalization(t *testing.T) {
	for input, want := range map[string]string{"Jibóia": "jiboia", "  LÍRIO-do  campo! ": "lirio do campo", "orquídeas 🌱": "orquideas", "a\u0301rvore": "arvore", "123 cactos": "123 cactos", " !!! ": ""} {
		if got := normalizeTerm(input); got != want {
			t.Errorf("%q -> %q want %q", input, got, want)
		}
	}
}
func TestFrontendBrowserPreflight(t *testing.T) {
	s := &Server{C: Config{Origins: "http://localhost:8081"}}
	for _, tc := range []struct {
		origin string
		want   int
	}{{"http://localhost:8081", 204}, {"https://other.example", 403}} {
		r := httptest.NewRequest(http.MethodOptions, "/v1/data/plants", nil)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("preflight %s: %d", tc.origin, w.Code)
		}
		if w.Code == 204 && w.Header().Get("Access-Control-Allow-Origin") != tc.origin {
			t.Fatal("missing CORS origin")
		}
	}
}
