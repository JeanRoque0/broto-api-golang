package api

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const searchPrompt = `Você recebe um termo de busca de planta em português do Brasil, possivelmente regional, no plural ou com erro de digitação. Devolva de seis a oito espécies diferentes, da mais provável para a menos provável, com nome científico e nome popular usado no Brasil. Quando o termo for gênero ou grupo, devolva espécies variadas. Use espécies conhecidas com verbete na Wikipédia. Não invente nome científico. Só plantas; para outros termos devolva lista vazia.`
const searchSchema = `{"type":"object","properties":{"especies":{"type":"array","items":{"type":"object","properties":{"cientifico":{"type":"string"},"popular":{"type":"string"}},"required":["cientifico","popular"],"additionalProperties":false}}},"required":["especies"],"additionalProperties":false}`

type species struct {
	Scientific string   `json:"scientific"`
	Common     string   `json:"common"`
	Extract    *string  `json:"extract"`
	Images     []string `json:"images"`
}

func (s *Server) fetchJSON(ctx context.Context, address string, out any) error {
	req, e := http.NewRequestWithContext(ctx, "GET", address, nil)
	if e != nil {
		return e
	}
	req.Header.Set("User-Agent", "Broto/1.0 (https://broto.app)")
	req.Header.Set("Api-User-Agent", "Broto/1.0 (https://broto.app)")
	res, e := s.HTTP.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return &httpStatusError{res.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(out)
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return http.StatusText(e.code) }
func usableImage(s string) bool {
	u, e := url.Parse(s)
	return e == nil && u.Scheme == "https" && u.Host != "" && !strings.HasSuffix(strings.ToLower(u.Path), ".svg") && !strings.HasSuffix(strings.ToLower(u.Path), ".svgz")
}

type wikiPage struct {
	Type    string `json:"type"`
	Extract string `json:"extract"`
	Titles  struct {
		Canonical string `json:"canonical"`
	} `json:"titles"`
	Thumbnail struct {
		Source string `json:"source"`
	} `json:"thumbnail"`
	Original struct {
		Source string `json:"source"`
	} `json:"originalimage"`
}

func (s *Server) wikiSummary(ctx context.Context, language, title string) *wikiPage {
	var p wikiPage
	if s.fetchJSON(ctx, "https://"+language+".wikipedia.org/api/rest_v1/page/summary/"+url.PathEscape(strings.ReplaceAll(title, " ", "_")), &p) != nil || p.Type == "disambiguation" {
		return nil
	}
	if p.Titles.Canonical == "" {
		p.Titles.Canonical = title
	}
	return &p
}
func (s *Server) describe(ctx context.Context, v species) species {
	v.Images = []string{}
	lang := "pt"
	p := s.wikiSummary(ctx, lang, v.Scientific)
	if p == nil {
		lang = "en"
		p = s.wikiSummary(ctx, lang, v.Scientific)
		if p == nil {
			return v
		}
		var links struct {
			Query struct {
				Pages map[string]struct {
					Links []map[string]string `json:"langlinks"`
				} `json:"pages"`
			} `json:"query"`
		}
		if s.fetchJSON(ctx, "https://en.wikipedia.org/w/api.php?action=query&format=json&redirects=1&prop=langlinks&lllang=pt&lllimit=1&titles="+url.QueryEscape(v.Scientific), &links) == nil {
			for _, page := range links.Query.Pages {
				if len(page.Links) > 0 {
					if pt := s.wikiSummary(ctx, "pt", page.Links[0]["*"]); pt != nil {
						p = pt
						lang = "pt"
						break
					}
				}
			}
		}
	}
	if lang == "pt" && p.Extract != "" {
		v.Extract = &p.Extract
	}
	seen := map[string]bool{}
	add := func(src string) {
		if strings.HasPrefix(src, "//") {
			src = "https:" + src
		}
		if len(v.Images) < 3 && usableImage(src) && !seen[src] {
			v.Images = append(v.Images, src)
			seen[src] = true
		}
	}
	if usableImage(p.Thumbnail.Source) {
		add(p.Thumbnail.Source)
	} else {
		add(p.Original.Source)
	}
	var media struct {
		Items []struct {
			Type   string `json:"type"`
			Srcset []struct {
				Src string `json:"src"`
			} `json:"srcset"`
		} `json:"items"`
	}
	if s.fetchJSON(ctx, "https://"+lang+".wikipedia.org/api/rest_v1/page/media-list/"+url.PathEscape(strings.ReplaceAll(p.Titles.Canonical, " ", "_")), &media) == nil {
		for _, item := range media.Items {
			if item.Type == "image" && len(item.Srcset) > 0 {
				add(item.Srcset[len(item.Srcset)-1].Src)
			}
		}
	}
	return v
}
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Term string `json:"term"`
	}
	if !body(w, r, &b) {
		return
	}
	term := normalizeTerm(b.Term)
	if len(term) > 120 {
		fail(w, 400, "termo_longo")
		return
	}
	empty := []species{}
	if len(term) < 3 {
		send(w, 200, map[string]any{"resultados": empty})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	var cached []byte
	e := s.DB.QueryRow(ctx, "select results from species_cache where term=$1", term).Scan(&cached)
	if e == nil {
		send(w, 200, map[string]any{"resultados": json.RawMessage(cached), "fonte": "cache"})
		return
	}
	if e != pgx.ErrNoRows {
		s.dbError(w, e)
		return
	}
	if s.C.AnthropicKey == "" {
		fail(w, 503, "ia_nao_configurada")
		return
	}
	// Global search spending is committed before the external call, including failed calls.
	var allowed bool
	if e = s.DB.QueryRow(ctx, "select bump_search_budget(1000)").Scan(&allowed); e != nil {
		s.dbError(w, e)
		return
	}
	if !allowed {
		send(w, 200, map[string]any{"resultados": empty, "fonte": "vazio"})
		return
	}
	text, _, e := s.ask(ctx, s.C.SearchModel, searchPrompt, searchSchema, []message{{"user", term}}, 800)
	if e != nil {
		fail(w, 502, "falha_busca")
		return
	}
	var parsed struct {
		Species []struct {
			Scientific string `json:"cientifico"`
			Common     string `json:"popular"`
		} `json:"especies"`
	}
	if json.Unmarshal([]byte(text), &parsed) != nil {
		fail(w, 502, "falha_busca")
		return
	}
	if len(parsed.Species) > 6 {
		parsed.Species = parsed.Species[:6]
	}
	out := make(chan species, len(parsed.Species))
	for _, v := range parsed.Species {
		go func(scientific, common string) {
			out <- s.describe(ctx, species{Scientific: scientific, Common: common})
		}(v.Scientific, v.Common)
	}
	byName := map[string]species{}
	for range parsed.Species {
		v := <-out
		byName[v.Scientific] = v
	}
	results := []species{}
	for _, v := range parsed.Species {
		if s := byName[v.Scientific]; len(s.Images) > 0 {
			results = append(results, s)
		}
	}
	if len(results) == 0 {
		send(w, 200, map[string]any{"resultados": results, "fonte": "vazio"})
		return
	}
	data, _ := json.Marshal(results)
	if _, e = s.DB.Exec(ctx, "insert into species_cache(term,results,source) values($1,$2,'modelo') on conflict(term) do update set results=excluded.results,source=excluded.source", term, data); e != nil {
		s.dbError(w, e)
		return
	}
	send(w, 200, map[string]any{"resultados": results, "fonte": "modelo"})
}
