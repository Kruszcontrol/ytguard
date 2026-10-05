// Package console is the YTGuard console: one web page for managing the
// kids on several YTGuard PCs, for families that don't use Home Assistant.
//
// It runs on the parent's own computer (Linux, Windows or Mac) with its own
// login, and talks to each kid PC's REST API with a token the PC issued
// when it was paired. Each PC's self-signed certificate is pinned at
// pairing time, so the console only ever talks to the real PC.
package console

import (
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ytguard"
	"ytguard/internal/admin"
	"ytguard/internal/auth"
	"ytguard/internal/pairing"
	"ytguard/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

const cookieName = "ytg_console"

// Server is the console web app.
type Server struct {
	St   *store.Store
	Auth *auth.Auth
	// DataDir holds the HTTPS certificate for phone access.
	DataDir string

	handlerOnce sync.Once
	handler     http.Handler
	rmu         sync.Mutex // phone access listener
	rsrv        *http.Server
	rport       int
	rerr        string
	rfp         string // its certificate's fingerprint

	mu      sync.Mutex // PC list
	cmu     sync.Mutex // clients
	clients map[string]*http.Client
	smu     sync.Mutex // last
	last    Snapshot
	pages   map[string]*template.Template
}

// New parses the templates.
func New(st *store.Store, a *auth.Auth) (*Server, error) {
	s := &Server{St: st, Auth: a, clients: map[string]*http.Client{}, pages: map[string]*template.Template{}}
	fm := admin.Funcs()
	fm["short"] = pairing.Short
	fm["pretty"] = pairing.Pretty
	fm["at"] = clockAt
	entries, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := strings.TrimSuffix(strings.TrimPrefix(e, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		t, err := template.New("layout.html").Funcs(fm).ParseFS(templateFS, "templates/layout.html", e)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return s, nil
}

// Handler returns the HTTP handler (the same one for every listener).
func (s *Server) Handler() http.Handler {
	s.handlerOnce.Do(func() { s.handler = s.routes() })
	return s.handler
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", admin.StaticHandler())
	mux.HandleFunc("GET /theme/{name}", func(w http.ResponseWriter, r *http.Request) {
		admin.ThemeHandler(r.TLS != nil)(w, r)
	})
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup", s.setupPost)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginPost)

	page := func(pattern string, h pageHandler) { mux.Handle(pattern, s.session(h)) }
	page("POST /logout", s.logout)
	page("GET /{$}", s.dashboard)
	page("POST /pcs/{pc}/kids/{kid}/{action}", s.kidAction)
	page("POST /pcs/{pc}/requests/{id}/{action}", s.requestAction)
	page("POST /pcs/{pc}/findings/dismiss", s.findingDismiss)
	page("GET /history", s.historyPage)
	page("GET /filters", s.filtersPage)
	page("POST /filters/add", s.filterAdd)
	page("POST /filters/sync", s.filterSync)
	page("POST /filters/remove", s.filterRemove)
	page("GET /lists", s.listsPage)
	page("POST /lists/apply", s.listApply)
	page("POST /lists/refresh", s.listRefresh)
	page("POST /lists/remove", s.listRemove)
	page("GET /pcs", s.pcsPage)
	page("POST /pcs/probe", s.pcProbe)
	page("POST /pcs/pair", s.pcPair)
	page("POST /pcs/{pc}/rename", s.pcRename)
	page("POST /pcs/{pc}/trust", s.pcTrust)
	page("POST /pcs/{pc}/remove", s.pcRemove)
	page("GET /settings", s.settingsPage)
	page("POST /settings/name", s.settingsName)
	page("POST /settings/phone", s.settingsPhone)
	page("POST /settings/password", s.settingsPassword)
	page("POST /settings/sessions/{id}/revoke", s.sessionRevoke)
	return s.headers(mux)
}

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only devices at home or on a VPN; never the open internet, even
		// if a router forwards the port by mistake.
		if !homeNetwork(net.ParseIP(clientIP(r))) {
			http.Error(w, "The YTGuard console only answers devices on the home network or a VPN.", http.StatusForbidden)
			return
		}
		// On this computer's own address, the request must name a local
		// host too. That stops web pages reaching the console through DNS
		// rebinding.
		if la, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr); ok && la.IP.IsLoopback() && !localHost(r.Host) {
			http.Error(w, "Open the console at http://127.0.0.1"+portOf(r.Host), http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https://i.ytimg.com data:; style-src 'self' 'unsafe-inline'; script-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		} else if r.URL.Query().Get("v") == admin.AssetVersion() {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func localHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// cgnat is 100.64.0.0/10, used by VPNs such as Tailscale.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// homeNetwork reports whether ip is this computer, the local network
// (private and link-local ranges) or a typical VPN range.
func homeNetwork(ip net.IP) bool {
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip))
}

func portOf(hostport string) string {
	if _, p, err := net.SplitHostPort(hostport); err == nil {
		return ":" + p
	}
	return ""
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func fromThisComputer(r *http.Request) bool {
	ip := net.ParseIP(clientIP(r))
	return ip != nil && ip.IsLoopback()
}

// ---- sessions ----

type req struct {
	*http.Request
	Session store.Session
	IP      string
}

type pageHandler func(w http.ResponseWriter, r *req)

func (s *Server) hasLogin() bool {
	_, _, err := s.St.Admin()
	return err == nil
}

func (s *Server) session(h pageHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hasLogin() {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		ip := clientIP(r)
		c, err := r.Cookie(cookieName)
		var se store.Session
		if err == nil {
			se, err = s.Auth.Session(c.Value, ip)
		}
		if err != nil {
			if r.Method == "GET" {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			} else {
				http.Error(w, "session expired — please log in again", http.StatusUnauthorized)
			}
			return
		}
		if r.Method == "POST" {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			if subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(se.CSRF)) != 1 {
				http.Error(w, "invalid form token — reload the page and try again", http.StatusForbidden)
				return
			}
		}
		h(w, &req{Request: r, Session: se, IP: ip})
	})
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, value string, expires time.Time, persistent bool) {
	c := &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode}
	if persistent {
		c.Expires = expires
	}
	if value == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, remember bool) error {
	ip := clientIP(r)
	device := admin.DeviceName(r.Context(), r.UserAgent(), ip)
	tok, se, err := s.Auth.NewSession(device, ip, truncate(r.UserAgent(), 200), remember, 90)
	if err != nil {
		return err
	}
	s.setCookie(w, r, tok, time.Unix(se.ExpiresAt, 0), remember)
	s.St.Audit("console", ip, "login", device)
	return nil
}

// ---- first run ----

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if s.hasLogin() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	st, _ := s.St.Settings()
	s.render(w, r, "setup", nil, map[string]any{"Local": fromThisComputer(r), "Name": st.PCName, "Error": ""})
}

func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if s.hasLogin() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Only someone at this computer may create the first login.
	if !fromThisComputer(r) {
		http.Error(w, "set up the console on the computer it runs on", http.StatusForbidden)
		return
	}
	name := truncate(strings.TrimSpace(r.FormValue("name")), 40)
	data := map[string]any{"Local": true, "Name": name, "Error": ""}
	err := func() error {
		if r.FormValue("password") != r.FormValue("confirm") {
			return errors.New("passwords don't match")
		}
		if err := s.Auth.SetPassword(r.FormValue("username"), r.FormValue("password")); err != nil {
			return err
		}
		st, _ := s.St.Settings()
		if name != "" {
			st.PCName = name
		}
		return s.St.SaveSettings(st)
	}()
	if err == nil {
		err = s.startSession(w, r, true)
	}
	if err != nil {
		data["Error"] = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, "setup", nil, data)
		return
	}
	s.St.Audit("console", clientIP(r), "console set up", "")
	q := "msg=" + url.QueryEscape("Welcome! Add your kids' PCs to get started.")
	if r.FormValue("phone") == "on" {
		if err := s.SetPhone(Phone{Enabled: true, Port: DefaultPhonePort}); err != nil {
			q = "err=" + url.QueryEscape("Phone access couldn't start: "+err.Error()+" — change it under Settings.")
		} else {
			q = "msg=" + url.QueryEscape("Welcome! Phone access is on — see Settings → Phone access for the address. Now add your kids' PCs.")
		}
	}
	http.Redirect(w, r, "/pcs?"+q, http.StatusSeeOther)
}

// ---- login ----

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.hasLogin() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", nil, map[string]any{"Next": admin.SafeNext(r.URL.Query().Get("next")), "Username": "", "Error": ""})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	ip := clientIP(r)
	user := r.FormValue("username")
	next := admin.SafeNext(r.FormValue("next"))
	if err := s.Auth.Login(user, r.FormValue("password"), ip); err != nil {
		s.St.Audit(user, ip, "login failed", err.Error())
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login", nil, map[string]any{"Next": next, "Username": user, "Error": err.Error()})
		return
	}
	if err := s.startSession(w, r, r.FormValue("remember") == "on"); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *req) {
	_ = s.St.DeleteSession(r.Session.ID)
	s.setCookie(w, r.Request, "", time.Time{}, false)
	http.Redirect(w, r.Request, "/login", http.StatusSeeOther)
}

// ---- rendering ----

type pageData struct {
	Theme   string
	Themes  []admin.Theme
	Title   string
	Active  string
	CSRF    string
	Msg     string
	Err     string
	Name    string // this console's name
	Version string
	Pending int
	D       any
}

var titles = map[string]string{
	"login": "Log in", "setup": "Set up", "dashboard": "Dashboard", "history": "History", "filters": "Filters",
	"lists": "Shared lists", "pcs": "PCs", "pcadd": "Add a PC", "settings": "Settings",
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, se *store.Session, d any) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "no template "+name, 500)
		return
	}
	st, _ := s.St.Settings()
	pd := pageData{Theme: admin.ThemeOf(r), Themes: admin.Themes(), Title: titles[name], Active: name, Name: st.PCName,
		Version: ytguard.Version, D: d, Msg: r.URL.Query().Get("msg"), Err: r.URL.Query().Get("err")}
	if se != nil {
		pd.CSRF = se.CSRF
		if snap, ok := s.cached(); ok {
			pd.Pending = len(snap.Requests)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, pd); err != nil {
		slog.Error("render", "page", name, "err", err)
	}
}

func (s *Server) page(w http.ResponseWriter, r *req, name string, d any) {
	s.render(w, r.Request, name, &r.Session, d)
}

// back redirects to the referring console page with a message.
func back(w http.ResponseWriter, r *req, fallback, msg string, err error) {
	target := fallback
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, e := url.Parse(ref); e == nil && u.Host == r.Host {
			target = u.Path
			q := u.Query()
			q.Del("msg")
			q.Del("err")
			if enc := q.Encode(); enc != "" {
				target += "?" + enc
			}
		}
	}
	target = admin.SafeNext(target)
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	if err != nil {
		target += sep + "err=" + url.QueryEscape(err.Error())
	} else if msg != "" {
		target += sep + "msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r.Request, target, http.StatusSeeOther)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// clockAt formats an RFC 3339 time as a clock time ("15:04").
func clockAt(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return t.Local().Format("15:04")
}
