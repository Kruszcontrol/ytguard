package filterlist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ytguard"
)

// AllowHTTP permits plain-http list URLs (tests only: lists must be
// HTTPS so nobody on the network can inject rules).
var AllowHTTP = false

var client = &http.Client{Timeout: 30 * time.Second}

// CheckURL validates a list or catalog URL.
func CheckURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return errors.New("not a valid web address")
	}
	if u.Scheme != "https" && !(AllowHTTP && u.Scheme == "http") {
		return errors.New("filter lists must use an https:// address")
	}
	if u.User != nil {
		return errors.New("addresses with a username or password aren't allowed")
	}
	return nil
}

// Fetched is a download result.
type Fetched struct {
	Text         string
	ETag         string
	LastModified string
	NotModified  bool
}

// Fetch downloads a list, sending cache validators so unchanged lists
// aren't downloaded again.
func Fetch(ctx context.Context, rawURL, etag, lastModified string) (Fetched, error) {
	var f Fetched
	if err := CheckURL(rawURL); err != nil {
		return f, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return f, err
	}
	req.Header.Set("User-Agent", "ytguard/"+ytguard.Version+" (filter lists)")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
	resp, err := client.Do(req)
	if err != nil {
		return f, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		f.NotModified = true
		return f, nil
	default:
		return f, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
	if err != nil {
		return f, err
	}
	if len(body) > MaxBytes {
		return f, fmt.Errorf("list is larger than %d MB", MaxBytes>>20)
	}
	f.Text, f.ETag, f.LastModified = string(body), resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	return f, nil
}

// Catalog is an index of recommended lists (catalog.json).
type Catalog struct {
	Lists []CatalogEntry `json:"lists"`
}

// CatalogEntry describes one list in a catalog.
type CatalogEntry struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Ages        string   `json:"ages"`
	URL         string   `json:"url"`
	Tags        []string `json:"tags,omitempty"`
}

// FetchCatalog downloads and validates a catalog.
func FetchCatalog(ctx context.Context, rawURL string) (Catalog, error) {
	var c Catalog
	f, err := Fetch(ctx, rawURL, "", "")
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal([]byte(f.Text), &c); err != nil {
		return c, fmt.Errorf("catalog isn't valid JSON: %v", err)
	}
	var ok []CatalogEntry
	for _, e := range c.Lists {
		if e.Title != "" && CheckURL(e.URL) == nil && len(ok) < 500 {
			e.Title, e.Description, e.Ages = truncate(e.Title, 120), truncate(e.Description, 500), truncate(e.Ages, 40)
			ok = append(ok, e)
		}
	}
	c.Lists = ok
	return c, nil
}
