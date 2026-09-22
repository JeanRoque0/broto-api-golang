package api

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestDocumentationAssets(t *testing.T) {
	h := (&Server{}).Routes()
	for _, path := range []string{"/docs/", "/docs/index.html", "/openapi.json", "/docs/initializer.js", "/docs/swagger-ui.css", "/docs/swagger-ui-bundle.js"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 200 || w.Body.Len() == 0 {
				t.Fatalf("%s: %d", path, w.Code)
			}
			if !strings.Contains(w.Header().Get("Content-Security-Policy"), "connect-src 'self'") {
				t.Fatal("missing CSP")
			}
			if path == "/openapi.json" && !json.Valid(w.Body.Bytes()) {
				t.Fatal("invalid JSON")
			}
			if strings.HasSuffix(path, ".js") && !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
				t.Fatal("invalid JS MIME")
			}
		})
	}
	for _, path := range []string{"/docs", "/swagger"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 307 || w.Header().Get("Location") != "/docs/" {
			t.Fatal("missing redirect", path)
		}
	}
	for _, path := range []string{"/docs/not-found", "/docs/VENDOR.md", "/docs/openapi.json"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatal("unexpected asset", path, w.Code)
		}
	}
	for header, compressed := range map[string]bool{"gzip": true, "br, gzip;q=0.5": true, "gzip;q=0": false, "identity": false, "gzip;q=bad": false, "": false} {
		r := httptest.NewRequest("GET", "/docs/swagger-ui-bundle.js", nil)
		r.Header.Set("Accept-Encoding", header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if (w.Header().Get("Content-Encoding") == "gzip") != compressed {
			t.Fatal("encoding", header)
		}
		data := w.Body.Bytes()
		if compressed {
			z, e := gzip.NewReader(w.Body)
			if e != nil {
				t.Fatal(e)
			}
			data, e = io.ReadAll(z)
			z.Close()
			if e != nil {
				t.Fatal(e)
			}
		}
		if !strings.Contains(string(data), "SwaggerUIBundle") {
			t.Fatal("corrupt bundle")
		}
	}
}

func TestDocumentationPublicOrigin(t *testing.T) {
	s := &Server{C: Config{PublicURL: "https://api.example.invalid/base", Origins: "https://app.example.invalid"}}
	for origin, status := range map[string]int{"https://api.example.invalid": 200, "https://app.example.invalid": 200, "https://evil.example.invalid": 403, "null": 403, "http://api.example.invalid": 403} {
		r := httptest.NewRequest("GET", "/openapi.json", nil)
		r.Header.Set("Origin", origin)
		r.Host = "evil.example.invalid"
		r.Header.Set("X-Forwarded-Host", "evil.example.invalid")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s: %d", origin, w.Code)
		}
	}
}

func TestOpenAPIResourceContract(t *testing.T) {
	data, err := documentation.ReadFile("docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string           `json:"operationId"`
			Security    []map[string]any `json:"security"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	h := (&Server{}).Routes()
	for path, methods := range spec.Paths {
		for method, operation := range methods {
			if operation.OperationID == "" || ids[operation.OperationID] {
				t.Fatal("duplicate/missing operationId", path)
			}
			ids[operation.OperationID] = true
			// All documented private operations must actually be mounted AND protected.
			if len(operation.Security) > 0 {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(strings.ToUpper(method), strings.ReplaceAll(path, "{id}", "test-id"), nil))
				if w.Code != 401 {
					t.Fatalf("unmounted or unprotected operation %s %s: %d", method, path, w.Code)
				}
			}
		}
	}
	for name, resource := range resources {
		fields := spec.Components.Schemas[name+"Write"].Properties
		if len(fields) != len(strings.Fields(resource.fields)) {
			t.Fatal("stale fields", name)
		}
		for _, field := range strings.Fields(resource.fields) {
			if _, ok := fields[field]; !ok {
				t.Fatal("missing field", name, field)
			}
		}
		for _, method := range strings.Fields(resource.methods) {
			path := "/v1/data/" + name
			if name != "profiles" && (method == "PATCH" || method == "DELETE") {
				path += "/{id}"
			}
			if _, ok := spec.Paths[path][strings.ToLower(method)]; !ok {
				t.Fatal("undocumented method", method, path)
			}
		}
	}
}

func TestDocumentationAuthenticatedRequest(t *testing.T) {
	f := newFrontendHarness(t)
	_, token := f.account()
	r := httptest.NewRequest("POST", "/v1/data/plant_groups", strings.NewReader(`{"name":"Sala"}`))
	r.Header.Set("Origin", f.s.C.PublicURL)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("Swagger same-origin request: %d %s", w.Code, w.Body.String())
	}
	// The documentation's column snapshot must match fresh migrations, never a stale local DB.
	raw, err := os.ReadFile("../../docs/data-columns.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]map[string]struct {
		Type     string `json:"type"`
		Nullable bool   `json:"nullable"`
		Default  bool   `json:"default"`
		UDT      string `json:"udt"`
	}
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	for name := range resources {
		rows, err := f.s.DB.Query(t.Context(), "select column_name,data_type,is_nullable='YES',column_default is not null,udt_name from information_schema.columns where table_schema='public' and table_name=$1", name)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for rows.Next() {
			var column, kind, udt string
			var nullable, hasDefault bool
			if err = rows.Scan(&column, &kind, &nullable, &hasDefault, &udt); err != nil {
				t.Fatal(err)
			}
			want, ok := snapshot[name][column]
			if !ok || want.Type != kind || want.Nullable != nullable || want.Default != hasDefault || want.UDT != udt {
				t.Errorf("refresh docs/data-columns.json: %s.%s", name, column)
			}
			count++
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		if count != len(snapshot[name]) {
			t.Errorf("stale column snapshot: %s", name)
		}
	}
}
