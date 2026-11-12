package hamqtt

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ytguard/internal/mqtt"
	"ytguard/internal/update"
)

func TestUpdateInstallFromHA(t *testing.T) {
	b, _ := newBridge(t, "pcaaaaaaaaaa")
	base := "ytguard/pcaaaaaaaaaa"
	updateEntity := func() map[string]any {
		ents, _ := b.entities(base)
		for _, e := range ents {
			if e.component == "update" {
				return e.config
			}
		}
		t.Fatal("no update entity")
		return nil
	}
	if _, ok := updateEntity()["command_topic"]; ok {
		t.Fatal("Install offered without the helper")
	}

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(update.Release{Tag: "v9.9.9"})
	}))
	t.Cleanup(gh.Close)
	old := update.API
	update.API = gh.URL
	t.Cleanup(func() { update.API = old })
	b.App.Updates.Repo = "o/ytguard"
	dir := t.TempDir()
	unit := filepath.Join(dir, "ytguard-upgrade.path")
	os.WriteFile(unit, nil, 0o644)
	b.App.Installer = &update.Installer{DataDir: dir, UnitPath: unit, StatusPath: filepath.Join(dir, "status.json"), Now: time.Now}

	cfg := updateEntity()
	if cfg["command_topic"] != base+"/update/install" || cfg["payload_install"] != "install" {
		t.Fatalf("update entity: %v", cfg)
	}
	req := filepath.Join(dir, update.RequestName)
	send := func(payload string, retain bool) {
		b.handle(base, "homeassistant", mqtt.Message{Topic: base + "/update/install", Payload: []byte(payload), Retain: retain})
	}
	send("install", true) // retained: ignored
	send("nope", false)
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(req); err == nil {
		t.Fatal("installed on a retained or wrong payload")
	}
	send("install", false)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(req); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no install request from Home Assistant")
		}
	}
	if !b.App.Installer.Status().Busy() {
		t.Fatal("install not reported as in progress")
	}
}
