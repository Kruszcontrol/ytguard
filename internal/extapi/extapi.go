// Package extapi is the loopback-only HTTP API the Chrome extension talks
// to. The kid is identified by the Linux user owning the client socket, so
// the extension holds no secrets. It has no admin functions.
package extapi

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"ytguard"
	"ytguard/internal/core"
	"ytguard/internal/crx"
	"ytguard/internal/peercred"
	"ytguard/internal/rules"
	"ytguard/internal/store"
	"ytguard/internal/timekeeper"
)

// Server serves the extension API.
type Server struct {
	App   *core.App
	Key   *rsa.PrivateKey
	Addr  string // listen address, e.g. 127.0.0.1:7878
	Dev   bool   // accept any chrome-extension origin (unpacked dev builds)
	extID string
	crx   []byte
	upd   []byte
}

// BaseURL is where the extension and Chrome reach the daemon.
func (s *Server) BaseURL() string { return "http://" + s.Addr }

// ExtensionID is the ID of the packed extension.
func (s *Server) ExtensionID() string { return s.extID }

// Prepare packs the extension.
func (s *Server) Prepare() error {
	s.extID = crx.ExtensionID(&s.Key.PublicKey)
	z, err := crx.Zip(ytguard.Extension, "extension", func(m map[string]any) {
		m["version"] = ytguard.ExtensionVersion()
		m["update_url"] = s.BaseURL() + "/ext/updates.xml"
	})
	if err != nil {
		return err
	}
	if s.crx, err = crx.Pack(z, s.Key); err != nil {
		return err
	}
	s.upd = crx.UpdateManifest(s.extID, s.BaseURL()+"/ext/ytguard.crx", ytguard.ExtensionVersion())
	return nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ext/updates.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Write(s.upd)
	})
	mux.HandleFunc("GET /ext/ytguard.crx", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-chrome-extension")
		w.Write(s.crx)
	})
	mux.HandleFunc("GET /api/config", s.kid(s.config))
	mux.HandleFunc("POST /api/check", s.kid(s.check))
	mux.HandleFunc("POST /api/filter", s.kid(s.filter))
	mux.HandleFunc("POST /api/heartbeat", s.kid(s.heartbeat))
	mux.HandleFunc("POST /api/request", s.kid(s.request))
	mux.HandleFunc("GET /api/request", s.kid(s.requestStatus))
	return mux
}

// who describes the caller. Kid is nil for Linux users that aren't kids.
type who struct {
	Kid      *store.Kid
	Unmapped string // "allow" or "block" for non-kid users
}

type kidHandler func(w http.ResponseWriter, r *http.Request, c who)

func (s *Server) kid(h kidHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only the extension may call the API: browsers send an Origin on
		// cross-origin requests, so web pages can't impersonate it.
		origin := r.Header.Get("Origin")
		if origin != "" && origin != "chrome-extension://"+s.extID && !(s.Dev && strings.HasPrefix(origin, "chrome-extension://")) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if r.Header.Get("X-YTGuard") != "1" {
			http.Error(w, "missing header", http.StatusForbidden)
			return
		}
		local := r.Context().Value(http.LocalAddrContextKey).(net.Addr).String()
		user, err := peercred.User(r.RemoteAddr, local)
		if err != nil {
			slog.Warn("identify client", "remote", r.RemoteAddr, "err", err)
			http.Error(w, "cannot identify user", http.StatusForbidden)
			return
		}
		c := who{}
		k, err := s.App.St.KidByUser(user)
		switch {
		case err == nil:
			c.Kid = &k
		case errors.Is(err, store.ErrNotFound):
			st, _ := s.App.St.Settings()
			c.Unmapped = st.UnmappedPolicy
			if c.Unmapped != "block" {
				c.Unmapped = "allow"
			}
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h(w, r, c)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// unrestricted is returned for non-kid users when policy is allow.
var unrestricted = map[string]any{"managed": false}

func (s *Server) config(w http.ResponseWriter, r *http.Request, c who) {
	if c.Kid == nil {
		if c.Unmapped == "allow" {
			writeJSON(w, unrestricted)
			return
		}
		writeJSON(w, map[string]any{"managed": true, "blockAll": true})
		return
	}
	st, err := s.App.Status(c.Kid.ID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"managed": true, "kid": c.Kid.Name, "options": c.Kid.Options, "status": st,
		"heartbeatSec": timekeeper.HeartbeatInterval})
}

type checkResp struct {
	Outcome string            `json:"outcome"` // play | block | hide
	Reason  string            `json:"reason,omitempty"`
	Title   string            `json:"title,omitempty"`
	Channel string            `json:"channel,omitempty"`
	Status  timekeeper.Status `json:"status"`
	Request string            `json:"request,omitempty"` // pending | approved | denied
	NoAsk   bool              `json:"noAsk,omitempty"`   // a parent setting decided; asking won't help
}

func (s *Server) check(w http.ResponseWriter, r *http.Request, c who) {
	var m rules.Meta
	if err := readJSON(r, &m); err != nil || m.VideoID == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if c.Kid == nil {
		if c.Unmapped == "allow" {
			writeJSON(w, checkResp{Outcome: rules.Play, Status: timekeeper.Status{Allowed: true, RemainingSec: -1, BreakInSec: -1, LimitSec: -1}})
		} else {
			writeJSON(w, checkResp{Outcome: rules.Hide})
		}
		return
	}
	k := *c.Kid
	d, m, err := s.App.Decide(r.Context(), k, m, true)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	st, _ := s.App.Status(k.ID)
	day := timekeeper.Day(s.App.Now())
	resp := checkResp{Outcome: d.Outcome, Status: st}
	switch d.Outcome {
	case rules.Hide:
		_ = s.App.St.AddEvent(k.ID, day, store.EventHidden, m, d.Reason)
		// No title, channel or reason: the kid shouldn't learn anything.
	case rules.Block:
		_ = s.App.St.AddEvent(k.ID, day, store.EventBlocked, m, d.Reason)
		resp.Reason, resp.Title, resp.Channel, resp.NoAsk = kidReason(d), m.Title, m.ChannelName, d.Final
		if req, err := s.App.St.LatestRequest(k.ID, m.VideoID); err == nil {
			resp.Request = req.Status
		}
	}
	writeJSON(w, resp)
}

// kidReason explains a block in kid-friendly words.
func kidReason(d rules.Decision) string {
	if d.Block == nil {
		return ""
	}
	if d.Final {
		return "Shorts are turned off."
	}
	if d.Block.Default || d.Block.Rule == nil {
		return "New videos need a parent's OK first."
	}
	switch d.Block.Rule.Type {
	case rules.TypeVideo:
		return "This video isn't allowed."
	case rules.TypeChannel:
		return "Videos from this channel aren't allowed."
	case rules.TypeKeyword:
		return "This video has a word that isn't allowed."
	case rules.TypeCategory:
		return "Videos in this category aren't allowed."
	case rules.TypeAttribute:
		return "This kind of video (" + rules.DescribeAttribute(d.Block.Rule.Value) + ") isn't allowed."
	}
	return "This video isn't allowed."
}

func (s *Server) filter(w http.ResponseWriter, r *http.Request, c who) {
	var req struct {
		Items []rules.Meta `json:"items"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Items) > 300 {
		http.Error(w, "bad request", 400)
		return
	}
	out := map[string]string{}
	if c.Kid == nil {
		v := "show"
		if c.Unmapped != "allow" {
			v = "hide"
		}
		for _, it := range req.Items {
			out[it.VideoID] = v
		}
		writeJSON(w, map[string]any{"results": out})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	s.App.Enrich(ctx, req.Items)
	for _, it := range req.Items {
		d, _, err := s.App.Decide(ctx, *c.Kid, it, false)
		if err != nil {
			out[it.VideoID] = "hide"
			continue
		}
		switch d.Outcome {
		case rules.Hide:
			out[it.VideoID] = "hide"
		case rules.Block:
			out[it.VideoID] = "locked"
		default:
			out[it.VideoID] = "show"
		}
	}
	writeJSON(w, map[string]any{"results": out})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, c who) {
	var req struct {
		rules.Meta
		Playing bool `json:"playing"`
		Start   bool `json:"start"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if c.Kid == nil {
		writeJSON(w, map[string]any{"status": timekeeper.Status{Allowed: c.Unmapped == "allow", RemainingSec: -1, BreakInSec: -1, LimitSec: -1}})
		return
	}
	// Re-check the video in case rules changed while it plays.
	outcome := rules.Play
	if req.VideoID != "" {
		d, _, err := s.App.Decide(r.Context(), *c.Kid, req.Meta, false)
		if err == nil {
			outcome = d.Outcome
		}
	}
	playing := req.Playing && outcome == rules.Play
	st, err := s.App.Heartbeat(*c.Kid, req.Meta, playing, req.Start)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"status": st, "outcome": outcome})
}

func (s *Server) request(w http.ResponseWriter, r *http.Request, c who) {
	var req struct {
		VideoID string `json:"videoId"`
		Message string `json:"message"`
	}
	if err := readJSON(r, &req); err != nil || req.VideoID == "" || c.Kid == nil {
		http.Error(w, "bad request", 400)
		return
	}
	rq, err := s.App.RequestAccess(r.Context(), *c.Kid, req.VideoID, req.Message)
	if errors.Is(err, core.ErrTooManyRequests) {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, map[string]any{"status": rq.Status})
}

func (s *Server) requestStatus(w http.ResponseWriter, r *http.Request, c who) {
	if c.Kid == nil {
		writeJSON(w, map[string]any{"status": ""})
		return
	}
	rq, err := s.App.St.LatestRequest(c.Kid.ID, r.URL.Query().Get("videoId"))
	if err != nil {
		writeJSON(w, map[string]any{"status": ""})
		return
	}
	writeJSON(w, map[string]any{"status": rq.Status})
}
