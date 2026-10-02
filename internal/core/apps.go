package core

import (
	"context"
	"log/slog"
	"os/user"
	"strconv"
	"time"

	"ytguard/internal/appscan"
	"ytguard/internal/store"
)

// How long a finding stays on the dashboard after it was last seen.
const (
	runningWindow   = 15 * time.Minute
	installedWindow = 3 * time.Hour // the root scan runs hourly
)

// KidUIDs maps the Linux uid of each kid account to the kid.
func (a *App) KidUIDs() map[int]store.Kid {
	kids, _ := a.St.Kids()
	out := map[int]store.Kid{}
	for _, k := range kids {
		if u, err := user.Lookup(k.LinuxUser); err == nil {
			if uid, err := strconv.Atoi(u.Uid); err == nil {
				out[uid] = k
			}
		}
	}
	return out
}

// RecordFindings stores sightings from a scan.
func RecordFindings(st *store.Store, found []appscan.Finding, now time.Time) error {
	for _, f := range found {
		if err := st.UpsertFinding(store.Finding{Key: f.Key(), Kind: f.Kind, UID: f.UID, App: f.App, Location: f.Where, How: f.How}, now.Unix()); err != nil {
			return err
		}
	}
	return nil
}

// FindingView is a finding with the kid's name, for display and the API.
type FindingView struct {
	store.Finding
	Kid     string `json:"kid"`
	Running bool   `json:"runningNow"`
}

// ActiveFindings lists recent, not-dismissed findings.
func (a *App) ActiveFindings() []FindingView {
	now := a.Now()
	all, err := a.St.Findings(now.Add(-installedWindow).Unix(), false)
	if err != nil {
		slog.Error("load findings", "err", err)
		return nil
	}
	uids := a.KidUIDs()
	var out []FindingView
	for _, f := range all {
		if f.Kind == appscan.KindRunning && now.Unix()-f.LastSeen > int64(runningWindow/time.Second) {
			continue
		}
		v := FindingView{Finding: f, Kid: "Everyone on this PC", Running: f.Kind == appscan.KindRunning && now.Unix()-f.LastSeen < 150}
		if k, ok := uids[f.UID]; ok {
			v.Kid = k.Name
		} else if f.UID >= 0 {
			v.Kid = "uid " + strconv.Itoa(f.UID)
		}
		out = append(out, v)
	}
	return out
}

// DismissFinding hides a finding for good (e.g. an app the parent allows).
func (a *App) DismissFinding(key string) error {
	t := true
	return a.St.SetFindingFlags(key, nil, &t)
}

// notifyFindings sends one Home Assistant event per new finding.
func (a *App) notifyFindings() {
	t := true
	for _, f := range a.ActiveFindings() {
		if f.Notified {
			continue
		}
		_ = a.St.SetFindingFlags(f.Key, &t, nil)
		a.Event("unapproved_app", map[string]any{"kid": f.Kid, "app": f.App, "where": f.Location, "how": f.How,
			"kind": f.Kind, "message": f.Kid + ": " + f.App + " (" + f.How + ") — " + f.Location})
	}
}

// RunAppMonitor checks kids' running programs every minute and sends alerts
// for anything new (including what the hourly root scan found).
func (a *App) RunAppMonitor(ctx context.Context, procRoot string) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if s, _ := a.St.Settings(); s.AppScan {
			uids := map[int]bool{}
			for uid := range a.KidUIDs() {
				uids[uid] = true
			}
			if len(uids) > 0 {
				if err := RecordFindings(a.St, appscan.Procs(procRoot, uids), a.Now()); err != nil {
					slog.Error("record findings", "err", err)
				}
			}
			a.notifyFindings()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
