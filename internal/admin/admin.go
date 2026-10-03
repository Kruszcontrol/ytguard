// Package admin serves the parent's web UI and the REST API used by Home
// Assistant. Browser access uses the app admin password plus long-lived
// "remember this device" sessions; the REST API uses scoped bearer tokens.
package admin

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"ytguard"
	"ytguard/internal/auth"
	"ytguard/internal/core"
	"ytguard/internal/rules"
	"ytguard/internal/store"
	"ytguard/internal/update"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const cookieName = "ytg_session"

// Server is the admin UI.
type Server struct {
	App  *core.App
	Auth *auth.Auth
	// TrustProxy makes X-Forwarded-For count as the client IP (only when
	// the admin listener is on loopback behind a reverse proxy).
	TrustProxy bool
	// MQTTStatus describes the Home Assistant MQTT connection (optional).
	MQTTStatus func() string
	// OnChange is called after a parent action changes kid state, so
	// Home Assistant gets the new state right away (optional).
	OnChange func()
	// PCID identifies this PC to Home Assistant.
	PCID string

	pages map[string]*template.Template
}

// assetVersion fingerprints the embedded static files, so browsers fetch
// new copies after an upgrade instead of using cached old ones.
var assetVersion = func() string {
	h := sha256.New()
	_ = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := staticFS.ReadFile(p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10]
}()

// New parses templates.
func New(app *core.App, a *auth.Auth) (*Server, error) {
	s := &Server{App: app, Auth: a, pages: map[string]*template.Template{}}
	entries, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := strings.TrimSuffix(strings.TrimPrefix(e, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", e)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return s, nil
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("GET /theme/{name}", s.setTheme)
	mux.HandleFunc("POST /login", s.loginPost)

	page := func(pattern string, h pageHandler) { mux.Handle(pattern, s.session(h)) }
	page("POST /logout", s.logout)
	page("GET /{$}", s.dashboard)
	page("POST /kids/{id}/bonus", s.kidAction)
	page("POST /kids/{id}/lock", s.kidAction)
	page("POST /kids/{id}/unlock", s.kidAction)
	page("POST /kids/{id}/endbreak", s.kidAction)
	page("POST /kids/{id}/reset", s.kidAction)
	page("POST /requests/{id}/approve", s.requestDecide)
	page("POST /requests/{id}/deny", s.requestDecide)
	page("POST /findings/dismiss", s.findingDismiss)
	page("GET /kids", s.kidsPage)
	page("POST /kids", s.kidCreate)
	page("GET /kids/{id}", s.kidPage)
	page("POST /kids/{id}", s.kidSave)
	page("POST /kids/{id}/delete", s.kidDelete)
	page("GET /filters", s.filtersPage)
	page("GET /filters/overview", s.overviewPage)
	page("GET /filters/lists", s.listsPage)
	page("GET /filters/lists/export.txt", s.listExport)
	page("GET /filters/lists/{id}", s.listEntries)
	page("POST /filters/lists/subscribe", s.listSubscribe)
	page("POST /filters/lists/{id}/options", s.listOptions)
	page("POST /filters/lists/{id}/refresh", s.listRefresh)
	page("POST /filters/lists/{id}/delete", s.listDelete)
	page("POST /filters/add", s.filterAdd)
	page("POST /filters/quick", s.filterQuick)
	page("POST /filters/{id}/move", s.filterMove)
	page("POST /filters/{id}/delete", s.filterDelete)
	page("GET /tester", s.testerPage)
	page("GET /history", s.historyPage)
	page("GET /report", s.reportPreview)
	page("POST /report/send", s.reportSend)
	page("GET /settings", s.settingsPage)
	page("POST /settings", s.settingsSave)
	page("POST /settings/test-email", s.testEmail)
	page("POST /settings/test-ha", s.testHA)
	page("POST /settings/check-update", s.checkUpdate)
	page("POST /settings/cleanup", s.cleanupNow)
	page("GET /export", s.export)
	page("POST /import", s.importRules)
	page("GET /security", s.securityPage)
	page("POST /security/password", s.passwordChange)
	page("POST /security/sessions/{id}/rename", s.sessionRename)
	page("POST /security/sessions/{id}/revoke", s.sessionRevoke)
	page("POST /security/sessions/revoke-all", s.sessionRevokeAll)
	page("POST /security/tokens", s.tokenCreate)
	page("POST /security/tokens/{id}/revoke", s.tokenRevoke)

	s.apiRoutes(mux)
	return s.headers(mux)
}

// headers adds security headers to every response.
func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st, _ := s.App.St.Settings()
		frame := "'none'"
		if o := strings.TrimSpace(st.EmbedOrigins); o != "" {
			frame = "'self' " + o
		} else {
			w.Header().Set("X-Frame-Options", "DENY")
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https://i.ytimg.com data:; style-src 'self' 'unsafe-inline'; script-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors "+frame)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		} else if r.URL.Query().Get("v") == assetVersion {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the caller's IP.
func (s *Server) clientIP(r *http.Request) string {
	if s.TrustProxy {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			return strings.TrimSpace(strings.Split(f, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- sessions ----

type req struct {
	*http.Request
	Session store.Session
	IP      string
}

type pageHandler func(w http.ResponseWriter, r *req)

func (s *Server) session(h pageHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIP(r)
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
			r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
			tok := r.Header.Get("X-CSRF-Token")
			if tok == "" {
				if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
					_ = r.ParseMultipartForm(4 << 20)
				}
				tok = r.FormValue("csrf")
			}
			if subtle.ConstantTimeCompare([]byte(tok), []byte(se.CSRF)) != 1 {
				http.Error(w, "invalid form token — reload the page and try again", http.StatusForbidden)
				return
			}
		}
		h(w, &req{Request: r, Session: se, IP: ip})
	})
}

func (s *Server) setCookie(w http.ResponseWriter, value string, expires time.Time, persistent bool) {
	st, _ := s.App.St.Settings()
	c := cookieName + "=" + value + "; Path=/; HttpOnly; Secure"
	if strings.TrimSpace(st.EmbedOrigins) != "" {
		// Needed for the UI inside a Home Assistant iframe.
		c += "; SameSite=None; Partitioned"
	} else {
		c += "; SameSite=Lax"
	}
	if persistent {
		c += "; Expires=" + expires.UTC().Format(http.TimeFormat)
	}
	if value == "" {
		c += "; Max-Age=0"
	}
	w.Header().Add("Set-Cookie", c)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", nil, map[string]any{"Next": safeNext(r.URL.Query().Get("next")), "Username": "", "Error": ""})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	ip := s.clientIP(r)
	user, pw := r.FormValue("username"), r.FormValue("password")
	remember := r.FormValue("remember") == "on"
	next := safeNext(r.FormValue("next"))
	data := map[string]any{"Next": next, "Username": user, "Error": ""}
	if err := s.Auth.Login(user, pw, ip); err != nil {
		s.App.St.Audit(user, ip, "login failed", err.Error())
		data["Error"] = err.Error()
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login", nil, data)
		return
	}
	st, _ := s.App.St.Settings()
	device := deviceName(r.Context(), r.UserAgent(), ip)
	tok, se, err := s.Auth.NewSession(truncate(device, 60), ip, truncate(r.UserAgent(), 200), remember, st.SessionDays)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.setCookie(w, tok, time.Unix(se.ExpiresAt, 0), remember)
	s.App.St.Audit("admin", ip, "login", fmt.Sprintf("device %q, remember=%v", device, remember))
	s.App.Event("admin_login", map[string]any{"device": device, "ip": ip})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *req) {
	_ = s.App.St.DeleteSession(r.Session.ID)
	s.setCookie(w, "", time.Time{}, false)
	http.Redirect(w, r.Request, "/login", http.StatusSeeOther)
}

func safeNext(n string) string {
	if n == "" || !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.Contains(n, "\\") {
		return "/"
	}
	return n
}

// deviceName names a new login's device without asking: browser and system
// from the User-Agent, plus the device's network name when the local DNS
// (usually the router) knows it, e.g. "Chrome on Android (pixel-8)". It can
// be renamed under Security.
func deviceName(ctx context.Context, ua, ip string) string {
	name := guessDevice(ua)
	if host := lanHostname(ctx, ip); host != "" {
		name += " (" + host + ")"
	}
	return truncate(name, 60)
}

func guessDevice(ua string) string {
	sys := "a computer"
	switch {
	case strings.Contains(ua, "Android"):
		sys = "Android"
	case strings.Contains(ua, "iPhone"):
		sys = "iPhone"
	case strings.Contains(ua, "iPad"):
		sys = "iPad"
	case strings.Contains(ua, "CrOS"):
		sys = "ChromeOS"
	case strings.Contains(ua, "Windows"):
		sys = "Windows"
	case strings.Contains(ua, "Mac OS"):
		sys = "Mac"
	case strings.Contains(ua, "Linux"):
		sys = "Linux"
	}
	if strings.Contains(ua, "HomeAssistant") || strings.Contains(ua, "Home Assistant") {
		return "Home Assistant app on " + sys
	}
	browser := "Browser"
	switch {
	case strings.Contains(ua, "Edg/"), strings.Contains(ua, "EdgA/"):
		browser = "Edge"
	case strings.Contains(ua, "SamsungBrowser"):
		browser = "Samsung Internet"
	case strings.Contains(ua, "OPR/"):
		browser = "Opera"
	case strings.Contains(ua, "Firefox/"), strings.Contains(ua, "FxiOS"):
		browser = "Firefox"
	case strings.Contains(ua, "CriOS"), strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	return browser + " on " + sys
}

// lanHostname asks DNS for the client's name (short form), giving up
// quickly so logging in never stalls.
func lanHostname(ctx context.Context, ip string) string {
	addr := net.ParseIP(ip)
	if addr == nil || addr.IsLoopback() {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(ctx, ip)
	if err != nil || len(names) == 0 {
		return ""
	}
	host := strings.TrimSuffix(names[0], ".")
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	if host == "" || strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil || strings.Count(host, "-") > 4 {
		return "" // ISP-style names like 192-168-1-5 aren't useful
	}
	return host
}

// ---- themes ----

type themeInfo struct {
	ID, Name string
	Swatch   template.CSS // constant values below, safe for a style attribute
}

// themes are defined in static/style.css; the choice is a per-device cookie.
var themes = []themeInfo{
	{"system", "Match device", "linear-gradient(135deg,#f4f6fb 50%,#161b24 50%)"},
	{"light", "Light", "#f4f6fb"},
	{"dark", "Dark", "#161b24"},
	{"ocean", "Ocean", "linear-gradient(135deg,#0b2c44,#22d3ee)"},
	{"sunset", "Sunset", "linear-gradient(135deg,#ff8a3d,#d6336c)"},
	{"forest", "Forest", "linear-gradient(135deg,#16251c,#84cc16)"},
}

const themeCookie = "ytg_theme"

func themeOf(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil {
		for _, t := range themes {
			if t.ID == c.Value {
				return t.ID
			}
		}
	}
	return "system"
}

// setTheme remembers the theme on this device (no login needed: it's
// only a display preference).
func (s *Server) setTheme(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, t := range themes {
		if t.ID == name {
			http.SetCookie(w, &http.Cookie{Name: themeCookie, Value: name, Path: "/", MaxAge: 5 * 365 * 24 * 3600,
				Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		}
	}
	next := "/"
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Host == r.Host {
			next = safeNext(u.RequestURI())
		}
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// ---- rendering ----

type pageData struct {
	Theme   string
	Themes  []themeInfo
	Title   string
	Active  string
	CSRF    string
	Msg     string
	Err     string
	PCName  string
	Version string
	Kids    []store.Kid
	Pending int // approval requests waiting (nav badge)
	Update  update.Status
	D       any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, se *store.Session, d any) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "no template "+name, 500)
		return
	}
	st, _ := s.App.St.Settings()
	kids, _ := s.App.St.Kids()
	pd := pageData{Theme: themeOf(r), Themes: themes, Title: titles[name], Active: name, PCName: st.PCName, Version: ytguard.Version, Kids: kids, D: d, Update: s.App.Updates.Status(),
		Msg: r.URL.Query().Get("msg"), Err: r.URL.Query().Get("err")}
	if se != nil {
		pd.CSRF = se.CSRF
		if rs, err := s.App.St.Requests(store.RequestPending, 100); err == nil {
			pd.Pending = len(rs)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, pd); err != nil {
		slog.Error("render", "page", name, "err", err)
	}
}

var titles = map[string]string{
	"login": "Log in", "dashboard": "Dashboard", "kids": "Kids", "kid": "Kid settings", "filters": "Filters",
	"overview": "Filter overview", "lists": "Filter lists", "list": "Filter list", "tester": "Rule tester", "history": "History", "settings": "Settings",
	"security": "Security", "token": "New API token",
}

func (s *Server) page(w http.ResponseWriter, r *req, name string, d any) {
	s.render(w, r.Request, name, &r.Session, d)
}

// back redirects to the referring page (or fallback) with a message.
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
	target = safeNext(target) // same-site paths only
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

var funcs = template.FuncMap{
	"mins": func(sec int) string {
		if sec < 0 {
			return "∞"
		}
		if sec < 3600 {
			return fmt.Sprintf("%d min", (sec+59)/60)
		}
		return fmt.Sprintf("%dh %02dm", sec/3600, (sec%3600+59)/60)
	},
	"clock": func(ts int64) string {
		if ts == 0 {
			return ""
		}
		return time.Unix(ts, 0).Format("15:04")
	},
	"datetime": func(ts int64) string {
		if ts == 0 {
			return "never"
		}
		return time.Unix(ts, 0).Format("Jan 2 15:04")
	},
	"pct": func(used, limit int) int {
		if limit <= 0 {
			return 0
		}
		return min(100, used*100/limit)
	},
	"join":     strings.Join,
	"describe": func(r rules.Rule) string { return r.Describe() },
	"attr":     rules.DescribeAttribute,
	"has": func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	},
	"kidName": func(kids []store.Kid, id int64) string {
		if id == 0 {
			return "All kids"
		}
		for _, k := range kids {
			if k.ID == id {
				return k.Name
			}
		}
		return "?"
	},
	"title": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
	"thumb": func(id string) string { return "https://i.ytimg.com/vi/" + url.PathEscape(id) + "/mqdefault.jpg" },
	"watch": func(id string) string { return "https://www.youtube.com/watch?v=" + url.QueryEscape(id) },
	"chanURL": func(id string) string {
		return "https://www.youtube.com/channel/" + url.PathEscape(id)
	},
	"weekday": func(i int) string { return time.Weekday(i).String() },
	"list":    func(v ...string) []string { return v },
	"asset":   func(p string) string { return "/static/" + p + "?v=" + assetVersion },
	// icon renders an SVG icon from static/icons.svg.
	"icon": func(name string) template.HTML {
		return template.HTML(`<svg class="i" aria-hidden="true"><use href="/static/icons.svg?v=` + assetVersion + `#` + template.HTMLEscapeString(name) + `"></use></svg>`)
	},
	// level turns a percentage into ok / warn / bad for colouring bars.
	"level": func(pct int) string {
		switch {
		case pct >= 90:
			return "bad"
		case pct >= 70:
			return "warn"
		}
		return "ok"
	},
	"has64": func(list []int64, v int64) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	},
	// qa builds the data for a quick-action button on the history page.
	"qa": func(csrf string, kid int64, tier, list, typ, value, label, text string) map[string]any {
		return map[string]any{"CSRF": csrf, "Kid": kid, "Tier": tier, "List": list, "Type": typ, "Value": value, "Label": label, "Text": text}
	},
}

// ---- helpers ----

func pathID(r *req) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func formInt(r *req, name string, def int) int {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// LinuxUsers lists human accounts (uid 1000–59999 with a login shell).
func LinuxUsers() []string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(sc.Text(), ":")
		if len(p) < 7 {
			continue
		}
		uid, err := strconv.Atoi(p[2])
		if err != nil || uid < 1000 || uid >= 60000 || strings.HasSuffix(p[6], "nologin") || strings.HasSuffix(p[6], "false") {
			continue
		}
		out = append(out, p[0])
	}
	sort.Strings(out)
	return out
}

var errBadID = errors.New("bad id")
