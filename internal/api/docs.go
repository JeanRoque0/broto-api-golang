package api

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Browser assets are compressed to keep the production executable small.
//
//go:embed docs/*
var documentation embed.FS

func registerDocumentation(m *http.ServeMux) {
	m.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusTemporaryRedirect)
	})
	m.HandleFunc("GET /swagger", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusTemporaryRedirect)
	})
	for route, file := range map[string]string{
		"/docs/{$}": "index.html", "/docs/index.html": "index.html",
		"/docs/initializer.js": "initializer.js", "/openapi.json": "openapi.json",
		"/docs/swagger-ui-bundle.js": "swagger-ui-bundle.js.gz",
		"/docs/swagger-ui.css":       "swagger-ui.css.gz",
		"/docs/LICENSE":              "LICENSE", "/docs/NOTICE": "NOTICE",
		"/docs/swagger-ui-bundle.js.LICENSE.txt": "swagger-ui-bundle.js.LICENSE.txt",
	} {
		m.HandleFunc("GET "+route, func(w http.ResponseWriter, r *http.Request) {
			data, err := documentation.ReadFile("docs/" + file)
			if err != nil {
				http.Error(w, "documentacao_indisponivel", 500)
				return
			}
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Cache-Control", "no-cache")
			switch file {
			case "index.html":
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
			case "openapi.json":
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
			case "initializer.js", "swagger-ui-bundle.js.gz":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			case "swagger-ui.css.gz":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			default:
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			}
			// Decompress only for clients without gzip support; browsers receive the
			// embedded bytes directly, without recompressing on every request.
			if file == "swagger-ui-bundle.js.gz" || file == "swagger-ui.css.gz" {
				w.Header().Add("Vary", "Accept-Encoding")
				if acceptsGzip(r.Header.Get("Accept-Encoding")) {
					w.Header().Set("Content-Encoding", "gzip")
				} else {
					reader, e := gzip.NewReader(bytes.NewReader(data))
					if e != nil {
						http.Error(w, "documentacao_indisponivel", 500)
						return
					}
					defer reader.Close()
					data, err = io.ReadAll(reader)
					if err != nil {
						http.Error(w, "documentacao_indisponivel", 500)
						return
					}
				}
			}
			http.ServeContent(w, r, file, time.Time{}, bytes.NewReader(data))
		})
	}
}

// The configured public origin is trusted, never a caller-controlled Host or
// X-Forwarded-Host. This lets same-origin Swagger requests pass the CORS gate.
func publicOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		if strings.TrimSpace(fields[0]) != "gzip" {
			continue
		}
		quality := 1.0
		for _, field := range fields[1:] {
			k, v, ok := strings.Cut(strings.TrimSpace(field), "=")
			if ok && k == "q" {
				n, err := strconv.ParseFloat(v, 64)
				if err != nil || n <= 0 || n > 1 {
					return false
				}
				quality = n
			}
		}
		return quality > 0
	}
	return false
}
