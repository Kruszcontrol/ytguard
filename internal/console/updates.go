package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// installResult is a PC's answer to POST /api/v1/update/install.
type installResult struct {
	Started bool   `json:"started"`
	Version string `json:"version"`
	Latest  string `json:"latest_version"`
}

// install asks one PC to install the latest YTGuard release. The PC's root
// helper downloads it from GitHub and checks it; the console only asks.
func (s *Server) install(ctx context.Context, p PC) (installResult, error) {
	var out installResult
	err := s.call(ctx, p, "POST", "/api/v1/update/install", nil, &out)
	if errors.Is(err, ErrTooOld) {
		err = errors.New("its YTGuard is too old to install updates from the console; on that PC run: sudo ytguard upgrade")
	}
	return out, err
}

// pcUpdate installs the latest release on one PC.
func (s *Server) pcUpdate(w http.ResponseWriter, r *req) {
	p, err := s.pcFrom(r)
	if err != nil {
		back(w, r, "/", "", err)
		return
	}
	ctx, cancel := s.actx(r)
	defer cancel()
	out, err := s.install(ctx, p)
	msg := p.Name + " is already up to date."
	if err == nil && out.Started {
		msg = fmt.Sprintf("Installing YTGuard %s on %s… It restarts on the way; this page shows when it's done.", out.Latest, p.Name)
		s.St.Audit("console", r.IP, "update install", p.Name+": "+out.Version+" → "+out.Latest)
	}
	if err != nil {
		err = fmt.Errorf("%s: %w", p.Name, err)
	}
	back(w, r, "/", msg, err)
}

// updateAll installs the latest release on every PC that has one waiting.
func (s *Server) updateAll(w http.ResponseWriter, r *req) {
	ctx, cancel := s.actx(r)
	defer cancel()
	var targets []PC
	for _, v := range s.fetch(ctx).PCs {
		if v.CanInstall() {
			targets = append(targets, v.PC)
		}
	}
	if len(targets) == 0 {
		back(w, r, "/", "No PCs are waiting for an update.", nil)
		return
	}
	res := &result{}
	forEach(targets, func(p PC) struct{} {
		out, err := s.install(ctx, p)
		switch {
		case err != nil:
			res.add(&res.errs, p.Name+": "+err.Error())
		case out.Started:
			res.add(&res.done, p.Name)
		default:
			res.add(&res.same, p.Name)
		}
		return struct{}{}
	})
	var parts []string
	if len(res.done) > 0 {
		s.St.Audit("console", r.IP, "update install", strings.Join(res.done, ", "))
		parts = append(parts, "Installing the update on "+strings.Join(res.done, ", ")+"…")
	}
	if len(res.same) > 0 {
		parts = append(parts, "Already up to date: "+strings.Join(res.same, ", ")+".")
	}
	var err error
	if len(res.errs) > 0 {
		err = errors.New(strings.Join(res.errs, "; "))
	}
	back(w, r, "/", strings.Join(parts, " "), err)
}
