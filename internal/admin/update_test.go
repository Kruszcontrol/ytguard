package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ytguard"
	"ytguard/internal/auth"
	"ytguard/internal/update"
)

// fakeRelease makes the update checker see tag as GitHub's latest release.
func fakeRelease(t *testing.T, e *env, tag string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/ytguard/releases/latest" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(update.Release{Tag: tag, URL: "https://example/rel"})
	}))
	t.Cleanup(srv.Close)
	old := update.API
	update.API = srv.URL
	t.Cleanup(func() { update.API = old })
	e.app.Updates.Repo = "o/ytguard"
}

func TestOneClickInstall(t *testing.T) {
	e := setup(t)
	csrf := e.login()
	fakeRelease(t, e, "v9.9.9")

	// Before the helper is installed: the old instructions, no button.
	e.post("/settings/check-update", url.Values{"csrf": {csrf}})
	if _, body := e.get("/"); !strings.Contains(body, "v9.9.9 is available") || !strings.Contains(body, "sudo ytguard upgrade") || strings.Contains(body, "/settings/install-update") {
		t.Fatalf("dashboard without helper:\n%s", body)
	}
	if _, loc := e.post("/settings/install-update", url.Values{"csrf": {csrf}}); !strings.Contains(loc, "err=") {
		t.Fatalf("install without helper: %s", loc)
	}

	dir := t.TempDir()
	unit := filepath.Join(dir, "ytguard-upgrade.path")
	os.WriteFile(unit, nil, 0o644)
	e.app.Installer = &update.Installer{DataDir: dir, UnitPath: unit, StatusPath: filepath.Join(dir, "status.json"), Now: time.Now}

	if _, body := e.get("/"); !strings.Contains(body, `action="/settings/install-update"`) {
		t.Fatalf("no install button:\n%s", body)
	}
	if _, loc := e.post("/settings/install-update", url.Values{"csrf": {csrf}}); !strings.Contains(loc, "Installing") {
		t.Fatalf("install: %s", loc)
	}
	if _, err := os.Stat(filepath.Join(dir, update.RequestName)); err != nil {
		t.Fatal("no request file for the helper")
	}
	if _, body := e.get("/"); !strings.Contains(body, "Installing the YTGuard update") || !strings.Contains(body, "data-autorefresh") || strings.Contains(body, `action="/settings/install-update"`) {
		t.Fatalf("dashboard while installing:\n%s", body)
	}
	if _, loc := e.post("/settings/install-update", url.Values{"csrf": {csrf}}); !strings.Contains(loc, "err=") {
		t.Fatalf("second install accepted: %s", loc)
	}

	// API: admin scope only; reports the install state.
	do := func(method, path, scope string) (int, map[string]any) {
		tok, _ := e.au.NewToken("t", []string{scope})
		req, _ := http.NewRequest(method, e.srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := e.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if c, _ := do("POST", "/api/v1/update/install", auth.ScopeControl); c != 403 {
		t.Fatalf("control token installed: %d", c)
	}
	if c, out := do("POST", "/api/v1/update/install", auth.ScopeAdmin); c != http.StatusConflict {
		t.Fatalf("busy install via API: %d %v", c, out)
	}
	_, st := do("GET", "/api/v1/state", auth.ScopeRead)
	yt := st["ytguard"].(map[string]any)
	if yt["install_supported"] != true || yt["install_state"] != "requested" {
		t.Fatalf("state: %v", yt)
	}

	// The helper failed: shown, and it can be tried again.
	os.Remove(filepath.Join(dir, update.RequestName))
	update.WriteInstallStatus(e.app.Installer.StatusPath, update.InstallStatus{State: update.InstallFailed, Message: "checksum mismatch", At: time.Now()})
	if _, body := e.get("/"); !strings.Contains(body, "didn't install") || !strings.Contains(body, "checksum mismatch") || !strings.Contains(body, `action="/settings/install-update"`) {
		t.Fatalf("dashboard after failure:\n%s", body)
	}
	if c, out := do("POST", "/api/v1/update/install", auth.ScopeAdmin); c != 200 || out["started"] != true {
		t.Fatalf("API install: %d %v", c, out)
	}

	// Nothing newer: not requested.
	os.Remove(filepath.Join(dir, update.RequestName))
	oldVer := ytguard.Version
	ytguard.Version = "v1.0.0"
	t.Cleanup(func() { ytguard.Version = oldVer })
	fakeRelease(t, e, "v0.0.1")
	if _, loc := e.post("/settings/install-update", url.Values{"csrf": {csrf}}); !strings.Contains(loc, "already+up+to+date") {
		t.Fatalf("install when up to date: %s", loc)
	}
	if _, err := os.Stat(filepath.Join(dir, update.RequestName)); err == nil {
		t.Fatal("requested an install with nothing newer")
	}
}
