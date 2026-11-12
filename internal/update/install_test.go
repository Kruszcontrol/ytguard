package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testInstaller(t *testing.T, supported bool) (*Installer, *time.Time) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	i := &Installer{DataDir: dir, UnitPath: filepath.Join(dir, "ytguard-upgrade.path"),
		StatusPath: filepath.Join(dir, "status", "status.json"), Now: func() time.Time { return now }}
	if supported {
		os.WriteFile(i.UnitPath, []byte("[Path]\n"), 0o644)
	}
	return i, &now
}

func TestInstallerNilAndUnsupported(t *testing.T) {
	var nilI *Installer
	if nilI.Supported() || nilI.Status().State != "" {
		t.Fatal("nil installer should do nothing")
	}
	i, _ := testInstaller(t, false)
	if i.Supported() {
		t.Fatal("supported without the helper unit")
	}
	if err := i.Request(); err == nil || !strings.Contains(err.Error(), "sudo ytguard upgrade") {
		t.Fatalf("request without helper: %v", err)
	}
	if _, err := os.Stat(filepath.Join(i.DataDir, RequestName)); err == nil {
		t.Fatal("request file written without helper")
	}
}

func TestInstallerLifecycle(t *testing.T) {
	i, now := testInstaller(t, true)
	req := filepath.Join(i.DataDir, RequestName)
	os.Chtimes(i.DataDir, *now, *now)

	if err := i.Request(); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(req, *now, *now)
	if st := i.Status(); st.State != InstallRequested || !st.Busy() {
		t.Fatalf("after request: %+v", st)
	}
	if err := i.Request(); !errors.Is(err, ErrInstallBusy) {
		t.Fatalf("second request: %v", err)
	}

	// The helper picks it up: removes the request and reports progress.
	os.Remove(req)
	WriteInstallStatus(i.StatusPath, InstallStatus{State: InstallRunning, From: "v1.0.0", At: *now})
	if st := i.Status(); st.State != InstallRunning {
		t.Fatalf("running: %+v", st)
	}
	// A run that never finished is reported as failed.
	*now = now.Add(30 * time.Minute)
	if st := i.Status(); st.State != InstallFailed || !strings.Contains(st.Message, "journalctl") {
		t.Fatalf("stale run: %+v", st)
	}

	WriteInstallStatus(i.StatusPath, InstallStatus{State: InstallDone, From: "v1.0.0", To: "v1.1.0", At: *now})
	if st := i.Status(); st.State != InstallDone || st.To != "v1.1.0" {
		t.Fatalf("done: %+v", st)
	}
	*now = now.Add(25 * time.Hour)
	if st := i.Status(); st.State != "" {
		t.Fatalf("old result still shown: %+v", st)
	}
	if err := i.Request(); err != nil {
		t.Fatalf("request after done: %v", err)
	}
}
