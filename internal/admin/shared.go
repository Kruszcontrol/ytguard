package admin

import (
	"context"
	"html/template"
	"io/fs"
	"net/http"
)

// Shared with the YTGuard console, which uses the same look and feel.

// Theme is a colour theme the parent can pick.
type Theme = themeInfo

// StaticHandler serves the embedded CSS, JavaScript and icons; mount it
// at /static/.
func StaticHandler() http.Handler {
	static, _ := fs.Sub(staticFS, "static")
	return http.StripPrefix("/static/", http.FileServer(http.FS(static)))
}

// AssetVersion fingerprints the static files for cache busting.
func AssetVersion() string { return assetVersion }

// Funcs returns a copy of the template helpers.
func Funcs() template.FuncMap {
	out := template.FuncMap{}
	for k, v := range funcs {
		out[k] = v
	}
	return out
}

// Themes lists the colour themes.
func Themes() []Theme { return themes }

// ThemeOf is the theme chosen on this device.
func ThemeOf(r *http.Request) string { return themeOf(r) }

// ThemeHandler remembers a theme choice and goes back.
func ThemeHandler(secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { setTheme(w, r, secure) }
}

// DeviceName names a newly signed-in device.
func DeviceName(ctx context.Context, ua, ip string) string { return deviceName(ctx, ua, ip) }

// SafeNext keeps a redirect target on this site.
func SafeNext(n string) string { return safeNext(n) }
