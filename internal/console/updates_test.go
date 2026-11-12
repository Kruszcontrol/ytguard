package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ytguard/internal/update"
)

func TestConsoleInstallUpdate(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(update.Release{Tag: "v9.9.9", URL: "https://example/rel"})
	}))
	t.Cleanup(gh.Close)
	old := update.API
	update.API = gh.URL
	t.Cleanup(func() { update.API = old })

	pc1 := newKidPC(t, "Den PC", "Alice")
	pc2 := newKidPC(t, "Bedroom PC", "Bob")
	c := newConsole(t)
	c.pair(pc1)
	c.pair(pc2)
	helper := func(k *kidPC) string { // sets up one-click install on a PC
		dir := t.TempDir()
		unit := filepath.Join(dir, "ytguard-upgrade.path")
		os.WriteFile(unit, nil, 0o644)
		k.app.Installer = &update.Installer{DataDir: dir, UnitPath: unit, StatusPath: filepath.Join(dir, "status.json"), Now: time.Now}
		return filepath.Join(dir, update.RequestName)
	}
	for _, k := range []*kidPC{pc1, pc2} {
		k.app.Updates.Repo = "o/ytguard"
		k.app.Updates.Check(context.Background())
	}
	req1 := helper(pc1) // Den PC has it; Bedroom PC still needs a manual upgrade
	var id1 string
	for _, p := range c.s.pcs() {
		if p.Name == "Den PC" {
			id1 = p.ID
		}
	}

	d := c.get("/")
	if !strings.Contains(d, `action="/pcs/`+id1+`/update"`) || !strings.Contains(d, "runs <code>sudo ytguard upgrade</code> on it once") || strings.Contains(d, "/updates/install") {
		t.Fatalf("dashboard:\n%s", d)
	}
	if _, body := c.post("/pcs/"+id1+"/update", url.Values{}); !strings.Contains(body, "Installing YTGuard v9.9.9 on Den PC") {
		t.Fatalf("install: %s", body)
	}
	if _, err := os.Stat(req1); err != nil {
		t.Fatal("Den PC got no install request")
	}
	if d := c.get("/"); !strings.Contains(d, "<b>Den PC</b> is installing YTGuard v9.9.9") {
		t.Fatalf("dashboard while installing:\n%s", d)
	}

	// "All PCs" skips the one already installing.
	req2 := helper(pc2)
	if _, body := c.post("/updates/install", url.Values{}); !strings.Contains(body, "Installing the update on Bedroom PC") || strings.Contains(body, "Den PC") {
		t.Fatalf("install all: %s", body)
	}
	if _, err := os.Stat(req2); err != nil {
		t.Fatal("Bedroom PC got no install request")
	}
	if _, body := c.post("/updates/install", url.Values{}); !strings.Contains(body, "No PCs are waiting") {
		t.Fatalf("install all again: %s", body)
	}
}
