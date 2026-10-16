package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// IndexNow tells Bing - and the search engines that read its index - which
// pages changed, instead of leaving them to its next crawl. The key file at
// /<key>.txt is how it knows the notice comes from whoever runs the site.
const indexNowEndpoint = "https://api.indexnow.org/indexnow"

// indexNowDelay gives a starting deployment time to be reachable from
// outside, since the search engine reads the key file before it accepts the
// notice.
const indexNowDelay = 30 * time.Second

func (a *App) indexNowPath() string { return "/" + a.cfg.IndexNowKey + ".txt" }

func (a *App) handleIndexNowKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", crawlerFileCache)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, a.cfg.IndexNowKey)
}

// NotifyIndexNow submits every indexable page once, as the service starts: a
// deployment is when they change. Without a key it does nothing.
func (a *App) NotifyIndexNow(ctx context.Context) {
	pages := a.indexablePages()
	if a.cfg.IndexNowKey == "" || len(pages) == 0 {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(indexNowDelay):
	}
	base := strings.TrimRight(a.cfg.PublicURL, "/")
	site, err := url.Parse(base)
	if err != nil {
		return
	}
	urls := make([]string, len(pages))
	for i, page := range pages {
		urls[i] = base + page
	}
	body, err := json.Marshal(map[string]any{
		"host":        site.Host,
		"key":         a.cfg.IndexNowKey,
		"keyLocation": base + a.indexNowPath(),
		"urlList":     urls,
	})
	if err != nil {
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, indexNowEndpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "indexnow: %v\n", err)
		}
		return
	}
	_ = response.Body.Close()
	fmt.Fprintf(os.Stderr, "indexnow: submitted %d pages: %s\n", len(urls), response.Status)
}
