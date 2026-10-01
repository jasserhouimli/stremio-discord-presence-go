// Package cinemeta fetches poster art from Stremio's public Cinemeta addon.
// No API key needed. Ported from src/cinemeta.js.
package cinemeta

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/macromaster101/stremio-discord-presence-go/internal/stremio"
)

const host = "v3-cinemeta.strem.io"

var (
	mu    sync.RWMutex
	cache = map[string]string{}
	// hasValue tracks null (missing) separately from empty-not-cached.
	nullCache = map[string]bool{}
)

type meta struct {
	Name        string `json:"name"`
	Poster      string `json:"poster"`
	ReleaseInfo string `json:"releaseInfo"`
	Year        string `json:"year"`
}

func get(path string) *struct {
	Metas []meta `json:"metas"`
} {
	c := &http.Client{Timeout: 4 * time.Second}
	req, err := http.NewRequest("GET", "https://"+host+path, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil
	}
	var out struct {
		Metas []meta `json:"metas"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil
	}
	return &out
}

func pickBest(metas []meta, m *stremio.MediaInfo) *meta {
	if len(metas) == 0 {
		return nil
	}
	wanted := strings.ToLower(strings.TrimSpace(m.SearchName))
	if wanted == "" {
		wanted = strings.ToLower(strings.TrimSpace(m.Name))
	}
	var pool []meta
	for _, mm := range metas {
		if strings.ToLower(strings.TrimSpace(mm.Name)) == wanted {
			pool = append(pool, mm)
		}
	}
	if len(pool) == 0 {
		pool = metas
	}
	if m.Year != "" {
		for i, mm := range pool {
			if strings.Contains(mm.ReleaseInfo, m.Year) || strings.Contains(mm.Year, m.Year) {
				out := pool[i]
				return &out
			}
		}
	}
	out := pool[0]
	return &out
}

func isHTTPSURL(v string) bool {
	if v == "" || len(v) > 512 {
		return false
	}
	u, err := url.Parse(v)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Host != ""
}

// GetPosterURL returns a poster image URL for the media, or "" on any miss.
func GetPosterURL(m *stremio.MediaInfo) string {
	if m == nil {
		return ""
	}
	query := strings.TrimSpace(m.SearchName)
	if query == "" {
		query = strings.TrimSpace(m.Name)
	}
	if query == "" {
		return ""
	}
	key := m.Type + "|" + query + "|" + m.Year
	mu.RLock()
	if v, ok := cache[key]; ok {
		mu.RUnlock()
		return v
	}
	if nullCache[key] {
		mu.RUnlock()
		return ""
	}
	mu.RUnlock()

	catalog := "movie"
	if m.Type == "series" {
		catalog = "series"
	}
	path := "/catalog/" + catalog + "/top/search=" + url.QueryEscape(query) + ".json"
	res := get(path)
	var best *meta
	if res != nil {
		best = pickBest(res.Metas, m)
	}
	if best == nil {
		alt := "series"
		if catalog == "series" {
			alt = "movie"
		}
		altPath := "/catalog/" + alt + "/top/search=" + url.QueryEscape(query) + ".json"
		if res2 := get(altPath); res2 != nil {
			best = pickBest(res2.Metas, m)
		}
	}
	found := ""
	if best != nil && isHTTPSURL(best.Poster) {
		found = best.Poster
	}
	mu.Lock()
	if found == "" {
		nullCache[key] = true
	} else {
		cache[key] = found
	}
	mu.Unlock()
	return found
}
