// Package appscan looks for browsers and YouTube apps other than the
// managed Chrome: running programs (from /proc, no privileges needed),
// and installed copies in kids' home folders and system-wide (needs root
// to read home folders).
//
// It can't stop these programs; it only reports them so a parent knows.
package appscan

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Finding kinds.
const (
	KindRunning   = "running"   // a process is running right now
	KindInstalled = "installed" // found on disk
)

// Finding is one detected app.
type Finding struct {
	Kind  string `json:"kind"`
	UID   int    `json:"uid"`   // owner/kid uid; -1 = available to everyone
	App   string `json:"app"`   // friendly name, e.g. "Firefox"
	Where string `json:"where"` // path, Flatpak ID, ...
	How   string `json:"how"`   // e.g. "portable download", "Flatpak (user)"
}

// Key identifies a finding for de-duplication.
func (f Finding) Key() string { return fmt.Sprintf("%s|%d|%s", f.Kind, f.UID, f.Where) }

// app names by executable base name (lower case, without "-bin"/".exe").
var executables = map[string]string{
	"firefox": "Firefox", "firefox-esr": "Firefox", "librewolf": "LibreWolf", "waterfox": "Waterfox",
	"floorp": "Floorp", "zen": "Zen Browser", "palemoon": "Pale Moon", "seamonkey": "SeaMonkey",
	"icecat": "GNU IceCat", "mullvad-browser": "Mullvad Browser", "tor": "Tor", "start-tor-browser": "Tor Browser",
	"torbrowser-launcher": "Tor Browser", "chrome": "Chrome (unmanaged copy)", "google-chrome": "Chrome (unmanaged copy)",
	"chromium": "Chromium", "chromium-browser": "Chromium", "brave": "Brave", "brave-browser": "Brave",
	"opera": "Opera", "vivaldi": "Vivaldi", "msedge": "Microsoft Edge", "microsoft-edge": "Microsoft Edge",
	"yandex-browser": "Yandex Browser", "thorium": "Thorium", "ungoogled-chromium": "Ungoogled Chromium",
	"epiphany": "GNOME Web", "falkon": "Falkon", "midori": "Midori", "qutebrowser": "qutebrowser",
	"konqueror": "Konqueror", "luakit": "luakit", "surf": "surf", "nyxt": "Nyxt", "ladybird": "Ladybird",
	"freetube": "FreeTube (YouTube app)", "yt-dlp": "yt-dlp (YouTube downloader)", "youtube-dl": "youtube-dl (YouTube downloader)",
	"mpv": "mpv (can play YouTube links)", "celluloid": "Celluloid (can play YouTube links)",
	"parabolic": "Parabolic (video downloader)", "tubefeeder": "Pipeline (YouTube app)", "pipeline": "Pipeline (YouTube app)",
	"newpipe": "NewPipe", "grayjay": "Grayjay (YouTube app)", "minitube": "Minitube (YouTube app)",
}

// Flatpak app IDs of browsers and YouTube apps.
var flatpaks = map[string]string{
	"org.mozilla.firefox": "Firefox", "io.gitlab.librewolf-community": "LibreWolf", "net.waterfox.waterfox": "Waterfox",
	"one.ablaze.floorp": "Floorp", "app.zen_browser.zen": "Zen Browser", "net.mullvad.MullvadBrowser": "Mullvad Browser",
	"org.torproject.torbrowser-launcher": "Tor Browser", "com.github.micahflee.torbrowser-launcher": "Tor Browser",
	"com.google.Chrome": "Chrome (Flatpak, unmanaged)", "org.chromium.Chromium": "Chromium",
	"io.github.ungoogled_software.ungoogled_chromium": "Ungoogled Chromium", "com.github.Eloston.UngoogledChromium": "Ungoogled Chromium",
	"com.brave.Browser": "Brave", "com.opera.Opera": "Opera", "com.vivaldi.Vivaldi": "Vivaldi",
	"com.microsoft.Edge": "Microsoft Edge", "ru.yandex.Browser": "Yandex Browser", "org.gnome.Epiphany": "GNOME Web",
	"org.kde.falkon": "Falkon", "org.midori_browser.Midori": "Midori", "org.qutebrowser.qutebrowser": "qutebrowser",
	"org.kde.konqueror": "Konqueror", "io.freetubeapp.FreeTube": "FreeTube (YouTube app)",
	"de.schmidhuberj.tubefeeder": "Pipeline (YouTube app)", "org.nickvision.tubeconverter": "Parabolic (video downloader)",
	"io.mpv.Mpv": "mpv (can play YouTube links)", "io.github.celluloid_player.Celluloid": "Celluloid (can play YouTube links)",
	"app.grayjay.Grayjay": "Grayjay (YouTube app)", "com.github.unrud.VideoDownloader": "Video Downloader",
	"io.github.aandrew_me.ytdn": "YTDownloader", "net.codelogistics.clipgrab": "ClipGrab (video downloader)",
}

// ManagedChromeDir is the policy-controlled Chrome; its processes are fine.
const ManagedChromeDir = "/opt/google/chrome/"

// Classify returns the app name for an executable path or name.
func Classify(pathOrName string) (string, bool) {
	base := strings.ToLower(filepath.Base(pathOrName))
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, "-bin")
	base = strings.TrimSuffix(base, "-wrapper")
	if app, ok := executables[base]; ok {
		return app, true
	}
	if strings.HasSuffix(base, ".appimage") {
		for exe, app := range executables {
			if len(exe) >= 4 && strings.Contains(base, exe) {
				return app + " (AppImage)", true
			}
		}
	}
	return "", false
}

// ---- running processes ----

// Procs lists processes owned by the given uids whose program looks like
// a browser or YouTube app, excluding the managed Chrome.
func Procs(procRoot string, uids map[int]bool) []Finding {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Finding
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		uid, ok := procUID(dir)
		if !ok || !uids[uid] {
			continue
		}
		argv0, comm := procNames(dir)
		if strings.HasPrefix(argv0, ManagedChromeDir) {
			continue
		}
		app, ok := Classify(argv0)
		if !ok {
			if app, ok = Classify(comm); !ok {
				continue
			}
		}
		where := argv0
		if where == "" || !strings.Contains(where, "/") {
			where = firstNonEmpty(argv0, comm)
		}
		f := Finding{Kind: KindRunning, UID: uid, App: app, Where: where, How: describeLocation(where)}
		// One entry per app and folder (firefox + firefox-bin, helpers, ...).
		if k := fmt.Sprint(uid, "|", app, "|", filepath.Dir(where)); !seen[k] {
			seen[k] = true
			out = append(out, f)
		}
	}
	return out
}

func procUID(dir string) (int, bool) {
	f, err := os.Open(filepath.Join(dir, "status"))
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				uid, err := strconv.Atoi(fields[0])
				return uid, err == nil
			}
		}
	}
	return 0, false
}

func procNames(dir string) (argv0, comm string) {
	if b, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		argv0 = string(bytes.SplitN(b, []byte{0}, 2)[0])
		// Some programs rewrite their cmdline into one space-separated string.
		if i := strings.Index(argv0, " --"); i > 0 {
			argv0 = argv0[:i]
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
		comm = strings.TrimSpace(string(b))
	}
	return argv0, comm
}

func describeLocation(p string) string {
	switch {
	case strings.HasPrefix(p, "/app/"):
		return "Flatpak"
	case strings.HasPrefix(p, "/snap/"):
		return "Snap"
	case strings.HasPrefix(p, "/tmp/.mount_"):
		return "AppImage"
	case strings.HasPrefix(p, "/home/"), strings.HasPrefix(p, "/tmp/"), strings.HasPrefix(p, "/var/tmp/"), strings.HasPrefix(p, "/dev/shm/"):
		return "portable copy in a user folder"
	case strings.HasPrefix(p, "/usr/"), strings.HasPrefix(p, "/opt/"), strings.HasPrefix(p, "/bin/"):
		return "installed on the system"
	}
	return "running program"
}

// ---- installed copies ----

// Limits keep a home scan bounded.
var (
	MaxEntries = 400_000
	MaxDepth   = 12
)

// archive name hints: downloads that unpack into a browser.
var archiveHints = []string{"firefox", "tor-browser", "torbrowser", "chrome", "chromium", "brave", "librewolf",
	"waterfox", "floorp", "zen.linux", "mullvad-browser", "opera", "vivaldi", "freetube", "yt-dlp", "thorium"}

var archiveExts = []string{".tar.bz2", ".tar.xz", ".tar.gz", ".tgz", ".zip", ".deb", ".rpm", ".appimage", ".flatpakref", ".flatpak"}

// Home scans one user's home folder. It never follows symlinks and only
// reads the first bytes of candidate files.
func Home(home string, uid int) []Finding {
	var out []Finding
	seen := map[string]bool{}
	add := func(f Finding) {
		if !seen[f.Key()] {
			seen[f.Key()] = true
			out = append(out, f)
		}
	}
	// Per-user Flatpak installs (no sudo needed on most distros).
	for _, id := range flatpakApps(filepath.Join(home, ".local/share/flatpak/app")) {
		if app, ok := flatpaks[id]; ok {
			add(Finding{Kind: KindInstalled, UID: uid, App: app, Where: id, How: "Flatpak installed for this user"})
		}
	}
	n := 0
	root := filepath.Clean(home)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		n++
		if n > MaxEntries {
			return fs.SkipAll
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if strings.Count(rel, string(filepath.Separator)) >= MaxDepth || rel == ".local/share/flatpak" ||
				rel == ".config/google-chrome" || rel == ".cache/google-chrome" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, sockets, ...
		}
		name := strings.ToLower(d.Name())
		if app, ok := Classify(name); ok && isProgram(p) {
			add(Finding{Kind: KindInstalled, UID: uid, App: app, Where: p, How: "portable copy in home folder"})
			return nil
		}
		for _, ext := range archiveExts {
			if !strings.HasSuffix(name, ext) {
				continue
			}
			for _, h := range archiveHints {
				if strings.Contains(name, h) {
					add(Finding{Kind: KindInstalled, UID: uid, App: archiveApp(h), Where: p, How: "downloaded installer/archive"})
					return nil
				}
			}
		}
		return nil
	})
	return out
}

func archiveApp(hint string) string {
	if app, ok := executables[hint]; ok {
		return app
	}
	switch hint {
	case "tor-browser", "torbrowser":
		return "Tor Browser"
	case "zen.linux":
		return "Zen Browser"
	}
	return hint
}

// isProgram reports whether p is an executable ELF binary or script.
func isProgram(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	head := make([]byte, 4)
	if _, err := f.Read(head); err != nil {
		return false
	}
	isELF := bytes.Equal(head, []byte{0x7f, 'E', 'L', 'F'})
	isScript := head[0] == '#' && head[1] == '!'
	return (isELF || isScript) && st.Mode().Perm()&0o111 != 0
}

func flatpakApps(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// System lists browsers installed for everyone that ordinary users can run
// (e.g. Mint's built-in Firefox), plus system-wide Flatpaks and Snaps.
func System() []Finding {
	var out []Finding
	seen := map[string]bool{}
	for _, dir := range []string{"/usr/bin", "/usr/local/bin", "/snap/bin"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			app, ok := Classify(e.Name())
			if !ok {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if real, err := filepath.EvalSymlinks(p); err == nil {
				// Launcher scripts (e.g. Mint's firefox.sh): the real binary next to
				// it decides whether others can run it.
				if bin := strings.TrimSuffix(real, ".sh"); bin != real {
					if _, err := os.Stat(bin); err == nil {
						real = bin
					}
				}
				if strings.HasPrefix(real, ManagedChromeDir) || strings.HasSuffix(real, "/google-chrome") {
					continue
				}
				if !othersCanRun(real) || seen[real] {
					continue
				}
				seen[real] = true
				out = append(out, Finding{Kind: KindInstalled, UID: -1, App: app, Where: real, How: "installed for everyone on this PC"})
			}
		}
	}
	for _, id := range flatpakApps("/var/lib/flatpak/app") {
		if app, ok := flatpaks[id]; ok {
			out = append(out, Finding{Kind: KindInstalled, UID: -1, App: app, Where: id, How: "Flatpak installed for everyone on this PC"})
		}
	}
	return out
}

// othersCanRun reports whether a user who isn't the owner or in the
// group can execute p (kids aren't in admin groups).
func othersCanRun(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().Perm()&0o001 != 0
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
