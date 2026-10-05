package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ytguard"
	"ytguard/internal/auth"
	"ytguard/internal/pairing"
	"ytguard/internal/report"
	"ytguard/internal/rules"
	"ytguard/internal/store"
	"ytguard/internal/timekeeper"
)

// Pairing with a YTGuard console, and the API calls the console uses on
// top of the Home Assistant ones (see api.go):
//
//	(none)  POST   /api/v1/pair                 {proof, name} → {token, …}
//	read    GET    /api/v1/history?kid=&day=
//	control POST   /api/v1/findings/dismiss     {key}
//	admin   POST   /api/v1/lists                {url, mode, kids}
//	admin   POST   /api/v1/lists/{id}           {enabled, mode, kids}
//	admin   POST   /api/v1/lists/{id}/refresh
//	admin   DELETE /api/v1/lists/{id}
//	read    POST   /api/v1/token/revoke         revokes the calling token

func (s *Server) consoleRoutes(mux *http.ServeMux) {
	h := func(pattern, scope string, fn apiHandler) { mux.Handle(pattern, s.bearer(scope, fn)) }
	mux.HandleFunc("POST /api/v1/pair", s.apiPair)
	h("GET /api/v1/history", auth.ScopeRead, s.apiHistory)
	h("POST /api/v1/findings/dismiss", auth.ScopeControl, s.apiFindingDismiss)
	h("POST /api/v1/lists", auth.ScopeAdmin, s.apiListSubscribe)
	h("POST /api/v1/lists/{id}", auth.ScopeAdmin, s.apiListOptions)
	h("POST /api/v1/lists/{id}/refresh", auth.ScopeAdmin, s.apiListRefresh)
	h("DELETE /api/v1/lists/{id}", auth.ScopeAdmin, s.apiListDelete)
	h("POST /api/v1/token/revoke", auth.ScopeRead, s.apiTokenRevoke)
}

// apiTokenRevoke lets a console that's removing this PC clean up its token.
func (s *Server) apiTokenRevoke(w http.ResponseWriter, r *http.Request, t store.APIToken) {
	if err := s.App.St.DeleteToken(t.ID); err != nil {
		apiError(w, 500, err.Error())
		return
	}
	s.App.St.Audit("api:"+t.Name, s.clientIP(r), "API token revoked by its user", t.Name)
	apiJSON(w, map[string]any{"ok": true})
}

// ---- pairing (Security page + API) ----

func (s *Server) pairPage(w http.ResponseWriter, r *req) {
	code, exp, ok := s.pair.Active(time.Now())
	s.page(w, r, "pair", map[string]any{"Active": ok, "Code": pairing.FormatCode(code), "Expires": exp.Unix(),
		"Fingerprint": pairing.Pretty(s.CertFingerprint), "Short": pairing.Short(s.CertFingerprint), "HTTPS": s.CertFingerprint != ""})
}

func (s *Server) pairStart(w http.ResponseWriter, r *req) {
	if s.CertFingerprint == "" {
		back(w, r, "/security", "", errors.New("pairing needs this PC to serve HTTPS itself; create an API token for the console instead"))
		return
	}
	s.pair.Start(time.Now())
	s.App.St.Audit("admin", r.IP, "console pairing code shown", "")
	http.Redirect(w, r.Request, "/security/pair", http.StatusSeeOther)
}

func (s *Server) pairCancel(w http.ResponseWriter, r *req) {
	s.pair.Cancel()
	back(w, r, "/security", "Pairing code cancelled.", nil)
}

// apiPair is the only API call without a token: the console proves it
// knows the code shown on the Security page and gets a token back.
func (s *Server) apiPair(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if err := s.Auth.Throttled(ip); err != nil {
		apiError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if s.CertFingerprint == "" {
		apiError(w, http.StatusConflict, "this PC doesn't serve HTTPS itself; use an API token instead")
		return
	}
	var in struct{ Proof, Name string }
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		apiError(w, 400, "bad request")
		return
	}
	if err := s.pair.Check(in.Proof, s.CertFingerprint, time.Now()); err != nil {
		s.Auth.Failed(ip)
		s.App.St.Audit("console", ip, "console pairing failed", err.Error())
		apiError(w, http.StatusForbidden, err.Error())
		return
	}
	name := truncate(strings.TrimSpace(in.Name), 30)
	if name == "" {
		name = "console"
	}
	tok, err := s.Auth.NewToken("Console: "+name, auth.Scopes)
	if err != nil {
		apiError(w, 500, err.Error())
		return
	}
	s.App.St.Audit("console", ip, "console paired", name)
	s.App.Event("console_paired", map[string]any{"console": name, "ip": ip})
	st, _ := s.App.St.Settings()
	apiJSON(w, map[string]any{"token": tok, "pc": st.PCName, "pc_id": s.PCID, "version": ytguard.Version})
}

// ---- history ----

func (s *Server) apiHistory(w http.ResponseWriter, r *http.Request, _ store.APIToken) {
	q := r.URL.Query()
	r.SetPathValue("kid", q.Get("kid"))
	k, err := s.kidFromPath(r)
	if err != nil {
		apiError(w, 404, "kid not found")
		return
	}
	day := q.Get("day")
	if _, err := time.Parse("2006-01-02", day); err != nil {
		day = timekeeper.Day(time.Now())
	}
	rep, err := report.Build(s.App.St, k, day)
	if err != nil {
		apiError(w, 500, err.Error())
		return
	}
	ws, _ := s.App.St.Watches(k.ID, day)
	evs, _ := s.App.St.Events(k.ID, day)
	if ws == nil {
		ws = []store.Watch{}
	}
	if evs == nil {
		evs = []store.Event{}
	}
	apiJSON(w, map[string]any{"kid": map[string]any{"id": k.ID, "name": k.Name}, "day": day,
		"used_minutes": rep.TotalMin, "limit_minutes": rep.LimitMin, "watches": ws, "events": evs, "requests": rep.Requests})
}

// ---- other apps ----

func (s *Server) apiFindingDismiss(w http.ResponseWriter, r *http.Request, t store.APIToken) {
	key := body(r)["key"]
	if key == "" {
		apiError(w, 400, "key is required")
		return
	}
	if err := s.App.DismissFinding(key); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	s.App.St.Audit("api:"+t.Name, s.clientIP(r), "app finding dismissed", key)
	s.changed()
	apiJSON(w, map[string]any{"ok": true})
}

// ---- shared lists ----

type listChange struct {
	URL     string   `json:"url"`
	Mode    string   `json:"mode"`
	Enabled *bool    `json:"enabled"`
	Kids    *[]int64 `json:"kids"` // empty = all kids
}

func decodeListChange(w http.ResponseWriter, r *http.Request) (listChange, bool) {
	var in listChange
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		apiError(w, 400, "body must be JSON")
		return in, false
	}
	return in, true
}

func (s *Server) validKids(ids []int64) ([]int64, error) {
	for _, id := range ids {
		if _, err := s.App.St.Kid(id); err != nil {
			return nil, errors.New("unknown kid " + strconv.FormatInt(id, 10))
		}
	}
	return ids, nil
}

func (s *Server) apiListSubscribe(w http.ResponseWriter, r *http.Request, t store.APIToken) {
	in, ok := decodeListChange(w, r)
	if !ok {
		return
	}
	var kids []int64
	if in.Kids != nil {
		var err error
		if kids, err = s.validKids(*in.Kids); err != nil {
			apiError(w, 400, err.Error())
			return
		}
	}
	l, err := s.App.Subscribe(r.Context(), in.URL, kids, store.ValidListMode(in.Mode))
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	s.App.St.Audit("api:"+t.Name, s.clientIP(r), "list subscribed", l.Title)
	apiJSON(w, map[string]any{"ok": true, "list": l})
}

func (s *Server) apiListByID(w http.ResponseWriter, r *http.Request) (store.FilterList, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		var l store.FilterList
		if l, err = s.App.St.List(id); err == nil {
			return l, true
		}
	}
	apiError(w, 404, "list not found")
	return store.FilterList{}, false
}

func (s *Server) apiListOptions(w http.ResponseWriter, r *http.Request, t store.APIToken) {
	l, ok := s.apiListByID(w, r)
	if !ok {
		return
	}
	in, ok := decodeListChange(w, r)
	if !ok {
		return
	}
	kids, enabled, mode := l.Kids, l.Enabled, l.Mode
	if in.Kids != nil {
		var err error
		if kids, err = s.validKids(*in.Kids); err != nil {
			apiError(w, 400, err.Error())
			return
		}
	}
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if in.Mode != "" {
		mode = store.ValidListMode(in.Mode)
	}
	if err := s.App.St.SetListOptions(l.ID, kids, enabled, mode); err != nil {
		apiError(w, 500, err.Error())
		return
	}
	s.App.St.Audit("api:"+t.Name, s.clientIP(r), "list options", l.Title+" ("+mode+")")
	l, _ = s.App.St.List(l.ID)
	apiJSON(w, map[string]any{"ok": true, "list": l})
}

func (s *Server) apiListRefresh(w http.ResponseWriter, r *http.Request, _ store.APIToken) {
	l, ok := s.apiListByID(w, r)
	if !ok {
		return
	}
	if err := s.App.RefreshList(r.Context(), l.ID, true); err != nil {
		apiError(w, 502, err.Error())
		return
	}
	l, _ = s.App.St.List(l.ID)
	apiJSON(w, map[string]any{"ok": true, "list": l})
}

func (s *Server) apiListDelete(w http.ResponseWriter, r *http.Request, t store.APIToken) {
	l, ok := s.apiListByID(w, r)
	if !ok {
		return
	}
	if err := s.App.St.DeleteList(l.ID); err != nil {
		apiError(w, 500, err.Error())
		return
	}
	s.App.St.Audit("api:"+t.Name, s.clientIP(r), "list removed", l.Title)
	apiJSON(w, map[string]any{"ok": true})
}

// ---- rules ----

var (
	reBareVideo   = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	reBareChannel = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)
)

// apiResolve turns a pasted link or @handle in a video/channel rule into
// an ID, as the Filters page does. Bare IDs are kept as given, so a rule
// copied from another PC keeps its label without a lookup.
func (s *Server) apiResolve(r *http.Request, ru *rules.Rule) error {
	v := strings.TrimSpace(ru.Value)
	switch {
	case ru.Type == rules.TypeVideo && reBareVideo.MatchString(v),
		ru.Type == rules.TypeChannel && reBareChannel.MatchString(v):
		ru.Value = v
		return nil
	case ru.Type == rules.TypeVideo, ru.Type == rules.TypeChannel:
		return s.resolveRef(r.Context(), ru, v)
	}
	return nil
}
