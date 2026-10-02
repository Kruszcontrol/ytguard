// Package update checks GitHub for new ytguard releases and downloads
// them. Checking runs inside the daemon (no privileges needed); installing
// is done by an adult with "sudo ytguard upgrade".
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"ytguard"
)

// API is the GitHub API base URL (overridable for tests).
var API = "https://api.github.com"

// Release is a published GitHub release.
type Release struct {
	Tag       string    `json:"tag_name"`
	Name      string    `json:"name"`
	URL       string    `json:"html_url"`
	Notes     string    `json:"body"`
	Published time.Time `json:"published_at"`
	Assets    []Asset   `json:"assets"`
}

// Asset is a file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// BinaryName is the release asset for this machine.
func BinaryName() string { return "ytguard-linux-" + runtime.GOARCH }

var client = &http.Client{Timeout: 60 * time.Second}

// Latest fetches the newest non-prerelease release of repo ("owner/name").
func Latest(ctx context.Context, repo string) (Release, error) {
	var r Release
	if repo == "" {
		return r, errors.New("this build has no GitHub repository configured")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", API+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return r, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ytguard/"+ytguard.Version)
	resp, err := client.Do(req)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 404:
		return r, fmt.Errorf("no releases published yet for %s", repo)
	default:
		return r, fmt.Errorf("GitHub: HTTP %d", resp.StatusCode)
	}
	return r, json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r)
}

// Status is the result of the last check.
type Status struct {
	Enabled   bool      `json:"enabled"`
	Repo      string    `json:"repo"`
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	Available bool      `json:"available"`
	URL       string    `json:"url,omitempty"`
	Notes     string    `json:"notes,omitempty"`
	Checked   time.Time `json:"checked,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// Checker periodically looks for new releases.
type Checker struct {
	Repo    string
	Enabled func() bool
	// OnNew is called once per newly seen release that's newer than this build.
	OnNew func(Status)
	// Persist stores the last status so it survives restarts.
	Load func() (Status, string) // status, last notified tag
	Save func(Status, string)

	mu       sync.Mutex
	st       Status
	notified string
	loaded   bool
}

// Interval between automatic checks.
const Interval = 12 * time.Hour

// Status returns the last known status.
func (c *Checker) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	st := c.st
	st.Enabled, st.Repo, st.Current = c.Enabled(), c.Repo, ytguard.Version
	st.Available = st.Latest != "" && ytguard.Newer(st.Latest, ytguard.Version)
	return st
}

func (c *Checker) load() {
	if c.loaded || c.Load == nil {
		return
	}
	c.loaded = true
	c.st, c.notified = c.Load()
	if c.st.Repo != c.Repo { // saved by a build pointing at another repository
		c.st, c.notified = Status{}, ""
	}
}

// Check asks GitHub now.
func (c *Checker) Check(ctx context.Context) Status {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := Latest(ctx, c.Repo)
	c.mu.Lock()
	c.load()
	c.st.Checked, c.st.Repo = time.Now(), c.Repo
	if err != nil {
		c.st.Error = err.Error()
	} else {
		c.st.Error = ""
		c.st.Latest, c.st.URL, c.st.Notes = r.Tag, r.URL, truncate(r.Notes, 2000)
	}
	notify := err == nil && ytguard.Newer(r.Tag, ytguard.Version) && c.notified != r.Tag
	if notify {
		c.notified = r.Tag
	}
	if c.Save != nil {
		c.Save(c.st, c.notified)
	}
	c.mu.Unlock()
	st := c.Status()
	if notify && c.OnNew != nil {
		c.OnNew(st)
	}
	if err != nil {
		slog.Warn("update check", "err", err)
	}
	return st
}

// Run checks shortly after start and then every Interval while enabled.
func (c *Checker) Run(ctx context.Context) {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		next := Interval
		if c.Enabled() && c.Repo != "" {
			last := c.Status().Checked
			if wait := time.Until(last.Add(Interval)); !last.IsZero() && wait > 0 {
				next = wait // checked recently (e.g. before a restart)
			} else {
				c.Check(ctx)
			}
		}
		timer.Reset(next)
	}
}

// Download fetches this machine's binary for release r into dir, verifies
// it against the release's SHA256SUMS and returns its path.
func Download(ctx context.Context, r Release, dir string) (string, error) {
	var bin, sums *Asset
	for i := range r.Assets {
		switch r.Assets[i].Name {
		case BinaryName():
			bin = &r.Assets[i]
		case "SHA256SUMS":
			sums = &r.Assets[i]
		}
	}
	if bin == nil {
		return "", fmt.Errorf("release %s has no %s download", r.Tag, BinaryName())
	}
	if sums == nil {
		return "", fmt.Errorf("release %s has no SHA256SUMS file", r.Tag)
	}
	want, err := expectedSum(ctx, sums.URL, bin.Name)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "ytguard-download-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if err := get(ctx, bin.URL, io.MultiWriter(f, h), 200<<20); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(f.Name())
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", bin.Name, got, want)
	}
	if err := f.Chmod(0o755); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func expectedSum(ctx context.Context, url, name string) (string, error) {
	var b strings.Builder
	if err := get(ctx, url, &b, 1<<20); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS has no entry for %s", name)
}

func get(ctx context.Context, url string, w io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "ytguard/"+ytguard.Version)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, limit))
	return err
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
