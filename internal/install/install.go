package install

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"ytguard"
	"ytguard/internal/auth"
	"ytguard/internal/crx"
	"ytguard/internal/store"
)

// Fixed locations.
const (
	BinPath     = "/usr/local/bin/ytguard"
	DataDir     = "/var/lib/ytguard"
	UnitPath    = "/etc/systemd/system/ytguard.service"
	ServiceUser = "ytguard"
	ExtAddr     = "127.0.0.1:7878" // must match extension/background.js

	policyName    = "ytguard.json"
	blocklistName = "ytguard-blocklist.json"
)

// PolicyDirs are where Chrome and Chromium read managed policy on Linux.
var PolicyDirs = []string{"/etc/opt/chrome/policies/managed", "/etc/chromium/policies/managed"}

// Options for Install.
type Options struct {
	AdminAddr       string `json:"adminAddr"`       // e.g. ":8443"
	YouTubeRestrict int    `json:"youtubeRestrict"` // ForceYouTubeRestrict: 0 off, 1 moderate, 2 strict
	AllowExtensions bool   `json:"allowExtensions"` // don't block other Chrome extensions

	Upgrade        bool     `json:"-"` // reinstall with the saved options, no questions
	NonInteractive bool     `json:"-"`
	AdminUser      string   `json:"-"`
	AdminPassword  string   `json:"-"`
	Kids           []string `json:"-"` // "linuxuser:Name" pairs for non-interactive installs
}

const optionsFile = "install.json"

// SavedOptions returns the options of the previous install. Installs from
// before install.json existed are reconstructed from the unit and policy.
func SavedOptions() (Options, error) {
	var o Options
	if b, err := os.ReadFile(filepath.Join(DataDir, optionsFile)); err == nil {
		return o, json.Unmarshal(b, &o)
	}
	unit, err := os.ReadFile(UnitPath)
	if err != nil {
		return o, errors.New("YTGuard isn't installed on this PC; run 'sudo ./ytguard install' first")
	}
	o.AdminAddr = ":8443"
	f := strings.Fields(string(unit))
	for i, w := range f {
		if w == "--admin-addr" && i+1 < len(f) {
			o.AdminAddr = f[i+1]
		}
	}
	var pol map[string]any
	if b, err := os.ReadFile(filepath.Join(PolicyDirs[0], policyName)); err == nil && json.Unmarshal(b, &pol) == nil {
		if ext, ok := pol["ExtensionSettings"].(map[string]any); ok {
			_, blocked := ext["*"]
			o.AllowExtensions = !blocked
		}
		if r, ok := pol["ForceYouTubeRestrict"].(float64); ok {
			o.YouTubeRestrict = int(r)
		}
	}
	return o, nil
}

func saveOptions(o Options) error {
	b, _ := json.MarshalIndent(o, "", "  ")
	return os.WriteFile(filepath.Join(DataDir, optionsFile), b, 0o600)
}

// Prompter reads answers from the terminal.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
}

// NewPrompter uses stdin/stdout.
func NewPrompter() *Prompter { return &Prompter{in: bufio.NewReader(os.Stdin), out: os.Stdout} }

// Ask prompts with a default.
func (p *Prompter) Ask(q, def string) string {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", q, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", q)
	}
	line, _ := p.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// YesNo asks a yes/no question.
func (p *Prompter) YesNo(q string, def bool) bool {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	a := strings.ToLower(p.Ask(q+" ("+d+")", ""))
	if a == "" {
		return def
	}
	return strings.HasPrefix(a, "y")
}

// Password reads a password without echo.
func (p *Prompter) Password(q string) string {
	fmt.Fprint(p.out, q+": ")
	stty("-echo")
	line, _ := p.in.ReadString('\n')
	stty("echo")
	fmt.Fprintln(p.out)
	return strings.TrimRight(line, "\r\n")
}

func stty(arg string) {
	c := exec.Command("stty", arg)
	c.Stdin = os.Stdin
	_ = c.Run()
}

// NewPassword asks for a password twice until valid.
func (p *Prompter) NewPassword() string {
	for {
		pw := p.Password(fmt.Sprintf("New admin password (min %d characters)", auth.MinPasswordLen))
		if err := auth.ValidatePassword(pw); err != nil {
			fmt.Fprintln(p.out, err)
			continue
		}
		if p.Password("Repeat password") != pw {
			fmt.Fprintln(p.out, "Passwords don't match.")
			continue
		}
		return pw
	}
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// Install performs a full install or upgrade. Must run as root.
func Install(opt Options) error {
	if os.Geteuid() != 0 {
		return errors.New("run with sudo: sudo ./ytguard install")
	}
	p := NewPrompter()
	if opt.Upgrade {
		prev, err := SavedOptions()
		if err != nil {
			return err
		}
		prev.Upgrade, prev.NonInteractive = true, true
		opt = prev
	}
	if opt.AdminAddr == "" {
		opt.AdminAddr = ":8443"
	}

	// Binary.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	if self != BinPath {
		if err := copyFile(self, BinPath, 0o755); err != nil {
			return fmt.Errorf("install binary: %w", err)
		}
		fmt.Println("Installed", BinPath)
	}

	// Service user and data directory.
	if _, err := user.Lookup(ServiceUser); err != nil {
		if err := run("useradd", "--system", "--home-dir", DataDir, "--no-create-home", "--shell", "/usr/sbin/nologin", ServiceUser); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(DataDir, 0o700); err != nil {
		return err
	}

	st, err := store.Open(DataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	// Admin account.
	if _, _, err := st.Admin(); errors.Is(err, store.ErrNotFound) {
		if opt.Upgrade {
			return errors.New("no parent login found; run 'sudo ytguard install' instead of upgrading")
		}
		fmt.Println("\nCreate YTGuard's parent login. This is YTGuard's own account (not a Linux account);")
		fmt.Println("you'll use it to sign in from your phone or browser.")
		u, pw := opt.AdminUser, opt.AdminPassword
		if !opt.NonInteractive {
			u = p.Ask("Admin username", firstNonEmpty(u, "parent"))
			pw = p.NewPassword()
		}
		if err := auth.New(st).SetPassword(u, pw); err != nil {
			return err
		}
	}

	// Kids.
	if err := setupKids(st, p, opt); err != nil {
		return err
	}

	// Keys.
	key, err := ExtensionKey(DataDir)
	if err != nil {
		return err
	}
	cert, err := TLSCert(DataDir)
	if err != nil {
		return err
	}
	extID := crx.ExtensionID(&key.PublicKey)

	// Chrome policy.
	if !opt.NonInteractive && !opt.AllowExtensions {
		fmt.Println("\nChrome policy applies to every account on this PC.")
		opt.AllowExtensions = !p.YesNo("Block all other Chrome extensions (recommended on kids' PCs)?", true)
	}
	if err := WritePolicy(extID, opt); err != nil {
		return err
	}

	if err := saveOptions(opt); err != nil {
		return err
	}
	if err := FixOwnership(DataDir); err != nil {
		return err
	}

	// Service.
	if err := os.WriteFile(UnitPath, []byte(Unit(opt.AdminAddr)), 0o644); err != nil {
		return err
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "enable", "ytguard.service"); err != nil {
		return err
	}
	if err := run("systemctl", "restart", "ytguard.service"); err != nil {
		return err
	}

	if opt.Upgrade {
		fmt.Printf("\nYTGuard %s is running. Kids' Chrome picks up the new extension within a few hours,\nor right away after quitting and reopening Chrome.\n", ytguard.Version)
		return nil
	}
	port := opt.AdminAddr[strings.LastIndex(opt.AdminAddr, ":")+1:]
	host, _ := os.Hostname()
	fmt.Printf("\nYTGuard is running.\n\nOpen the parent UI from your phone or another computer:\n")
	fmt.Printf("  https://%s.local:%s\n", host, port)
	for _, ip := range LANAddrs() {
		if ip.To4() != nil {
			fmt.Printf("  https://%s:%s\n", ip, port)
		}
	}
	fmt.Printf("\nThe browser will warn about the self-signed certificate. Check that its SHA-256\nfingerprint is:\n  %s\n", Fingerprint(cert))
	fmt.Printf("\nChrome extension ID: %s\n", extID)
	fmt.Println("\nNext: have each kid quit Chrome completely and reopen it, then check chrome://policy")
	fmt.Println("and chrome://extensions (YTGuard should be listed as installed by your administrator).")
	return nil
}

func setupKids(st *store.Store, p *Prompter, opt Options) error {
	kids, err := st.Kids()
	if err != nil {
		return err
	}
	if opt.NonInteractive {
		for _, kv := range opt.Kids {
			u, name, _ := strings.Cut(kv, ":")
			if _, err := st.KidByUser(u); err == nil {
				continue
			}
			k := store.Kid{LinuxUser: u, Name: firstNonEmpty(name, u)}
			if err := st.SaveKid(&k); err != nil {
				return err
			}
		}
		return nil
	}
	if len(kids) > 0 {
		fmt.Printf("\n%d kid account(s) already set up; manage them in the web UI.\n", len(kids))
		return nil
	}
	fmt.Println("\nWhich Linux accounts on this PC belong to kids? (Other accounts won't be filtered.)")
	for _, u := range humanUsers() {
		if p.YesNo(fmt.Sprintf("  Is %q a kid's account?", u), false) {
			name := p.Ask("    Kid's name", strings.ToUpper(u[:1])+u[1:])
			k := store.Kid{LinuxUser: u, Name: name}
			if err := st.SaveKid(&k); err != nil {
				return err
			}
		}
	}
	return nil
}

func humanUsers() []string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) < 7 {
			continue
		}
		uid, _ := strconv.Atoi(parts[2])
		if uid >= 1000 && uid < 60000 && !strings.HasSuffix(parts[6], "nologin") && !strings.HasSuffix(parts[6], "false") {
			out = append(out, parts[0])
		}
	}
	return out
}

// Policy returns the managed Chrome policy for the extension ID.
func Policy(extID string, opt Options) map[string]any {
	ext := map[string]any{
		extID: map[string]any{
			"installation_mode": "force_installed",
			"update_url":        "http://" + ExtAddr + "/ext/updates.xml",
			"toolbar_pin":       "force_unpinned",
		},
	}
	if !opt.AllowExtensions {
		ext["*"] = map[string]any{"installation_mode": "blocked", "blocked_install_message": "Ask a parent to install extensions."}
	}
	p := map[string]any{
		"ExtensionSettings":          ext,
		"IncognitoModeAvailability":  1, // disabled
		"BrowserGuestModeEnabled":    false,
		"BrowserAddPersonEnabled":    false,
		"DeveloperToolsAvailability": 2, // blocks devtools and view-source
	}
	if opt.YouTubeRestrict > 0 {
		p["ForceYouTubeRestrict"] = opt.YouTubeRestrict
	}
	return p
}

// defaultBlocklist blocks alternative YouTube front-ends that would bypass
// the extension. Written once; edit the file to add more.
var defaultBlocklist = map[string]any{
	"URLBlocklist": []string{
		"yewtu.be", "inv.nadeko.net", "invidious.nerdvpn.de", "invidious.jing.rocks", "invidious.privacyredirect.com",
		"iv.ggtyler.dev", "invidious.f5.si", "inv.tux.pizza", "piped.video", "piped.kavin.rocks", "piped.private.coffee",
		"poketube.fun", "tube.cadence.moe", "viewtube.io", "youtube.googleapis.com", "ytb.trom.tf",
	},
}

// WritePolicy writes Chrome/Chromium managed policy files.
func WritePolicy(extID string, opt Options) error {
	data, _ := json.MarshalIndent(Policy(extID, opt), "", "  ")
	blk, _ := json.MarshalIndent(defaultBlocklist, "", "  ")
	wrote := false
	for i, dir := range PolicyDirs {
		// Chromium's directory only if Chromium is installed.
		if i > 0 {
			if _, err := os.Stat(filepath.Dir(filepath.Dir(dir))); err != nil {
				continue
			}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, policyName), data, 0o644); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(dir, blocklistName)); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(filepath.Join(dir, blocklistName), blk, 0o644); err != nil {
				return err
			}
		}
		fmt.Println("Wrote Chrome policy", filepath.Join(dir, policyName))
		wrote = true
	}
	if !wrote {
		return errors.New("no Chrome policy directory written")
	}
	return nil
}

// Unit is the systemd service definition.
func Unit(adminAddr string) string {
	return `[Unit]
Description=YTGuard family YouTube controls
After=network-online.target
Wants=network-online.target

[Service]
User=` + ServiceUser + `
Group=` + ServiceUser + `
ExecStart=` + BinPath + ` serve --data ` + DataDir + ` --admin-addr ` + adminAddr + `
Restart=always
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=` + DataDir + `
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
CapabilityBoundingSet=
UMask=0077

[Install]
WantedBy=multi-user.target
`
}

// Uninstall removes the service, policy and binary. purge also deletes data.
func Uninstall(purge bool) error {
	if os.Geteuid() != 0 {
		return errors.New("run with sudo")
	}
	_ = run("systemctl", "disable", "--now", "ytguard.service")
	_ = os.Remove(UnitPath)
	_ = run("systemctl", "daemon-reload")
	for _, dir := range PolicyDirs {
		_ = os.Remove(filepath.Join(dir, policyName))
		_ = os.Remove(filepath.Join(dir, blocklistName))
	}
	_ = os.Remove(BinPath)
	if purge {
		if err := os.RemoveAll(DataDir); err != nil {
			return err
		}
		_ = run("userdel", ServiceUser)
		fmt.Println("Removed", DataDir)
	} else {
		fmt.Println("Kept data in", DataDir, "(use --purge to delete it)")
	}
	fmt.Println("YTGuard uninstalled. Restart Chrome to drop the policy.")
	return nil
}

// FixOwnership gives the data directory to the service user (after root
// has written to it).
func FixOwnership(dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	u, err := user.Lookup(ServiceUser)
	if err != nil {
		return nil // dev setups without the service user
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
