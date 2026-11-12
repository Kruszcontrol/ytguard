package update

// One-click install.
//
// The daemon runs as an unprivileged user, so it can't replace itself.
// Instead it drops a request file in its data directory. A systemd path
// unit (written by "ytguard install") notices the file and starts
// ytguard-upgrade.service, which runs "ytguard upgrade --requested" as root.
//
// Root never reads the request file or anything else the daemon wrote: it
// only installs the newest GitHub release of the repository built into the
// binary, verified against the release's SHA256SUMS. So the worst a
// compromised daemon can do is ask for the latest official release.
// Root reports progress in a root-owned status file the daemon can read.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Fixed locations shared by the daemon and the root helper.
const (
	RequestName = "upgrade.request" // in the daemon's data directory
	HelperUnit  = "/etc/systemd/system/ytguard-upgrade.path"
	StatusPath  = "/var/lib/ytguard-upgrade/status.json"
)

// Install states.
const (
	InstallRequested = "requested" // waiting for the helper to pick it up
	InstallRunning   = "running"
	InstallDone      = "done"
	InstallFailed    = "failed"
)

// staleAfter is when a "running" or "requested" state is assumed dead
// (the helper's own timeout is 15 minutes).
const staleAfter = 20 * time.Minute

// recentFor is how long a finished install is still reported.
const recentFor = 24 * time.Hour

// ErrInstallBusy means an install is already under way.
var ErrInstallBusy = errors.New("an update is already being installed")

// InstallStatus is the last one-click install.
type InstallStatus struct {
	State   string    `json:"state,omitempty"` // "" = nothing recent
	From    string    `json:"from,omitempty"`
	To      string    `json:"to,omitempty"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at,omitempty"`
}

// Busy reports whether an install is waiting or running.
func (s InstallStatus) Busy() bool { return s.State == InstallRequested || s.State == InstallRunning }

// Installer is the daemon's side of one-click install.
type Installer struct {
	DataDir    string
	UnitPath   string // default HelperUnit
	StatusPath string // default StatusPath
	Now        func() time.Time
}

// NewInstaller uses the standard locations.
func NewInstaller(dataDir string) *Installer {
	return &Installer{DataDir: dataDir, UnitPath: HelperUnit, StatusPath: StatusPath, Now: time.Now}
}

// Supported reports whether the root helper is installed on this PC.
// Installs from before one-click install need one manual upgrade first.
func (i *Installer) Supported() bool {
	if i == nil || i.DataDir == "" {
		return false
	}
	_, err := os.Stat(i.UnitPath)
	return err == nil
}

func (i *Installer) requestPath() string { return filepath.Join(i.DataDir, RequestName) }

// Status combines the helper's status file with a pending request.
func (i *Installer) Status() InstallStatus {
	if i == nil {
		return InstallStatus{}
	}
	now := i.Now()
	st, _ := ReadInstallStatus(i.StatusPath)
	if fi, err := os.Stat(i.requestPath()); err == nil && !st.Busy() && now.Sub(fi.ModTime()) < staleAfter {
		return InstallStatus{State: InstallRequested, At: fi.ModTime()}
	}
	switch {
	case st.Busy() && now.Sub(st.At) > staleAfter:
		st = InstallStatus{State: InstallFailed, From: st.From, At: st.At,
			Message: "the install didn't finish; see: journalctl -u ytguard-upgrade"}
	case !st.Busy() && now.Sub(st.At) > recentFor:
		st = InstallStatus{}
	}
	return st
}

// Request asks the root helper to install the latest release.
func (i *Installer) Request() error {
	if !i.Supported() {
		return errors.New("one-click install isn't set up on this PC yet; run once: sudo ytguard upgrade")
	}
	if i.Status().Busy() {
		return ErrInstallBusy
	}
	// The content is only for people looking at the file; root ignores it.
	return os.WriteFile(i.requestPath(), []byte(i.Now().Format(time.RFC3339)+"\n"), 0o600)
}

// ReadInstallStatus reads the helper's status file.
func ReadInstallStatus(path string) (InstallStatus, error) {
	var st InstallStatus
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}

// WriteInstallStatus is used by the root helper. The directory is
// root-owned and world-readable, so the daemon can read but not change it.
func WriteInstallStatus(path string, st InstallStatus) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	tmp := path + ".new"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
