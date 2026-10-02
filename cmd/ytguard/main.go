// Command ytguard is a family YouTube control daemon for Linux + Chrome.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ytguard"
	"ytguard/internal/admin"
	"ytguard/internal/auth"
	"ytguard/internal/core"
	"ytguard/internal/crx"
	"ytguard/internal/extapi"
	"ytguard/internal/install"
	"ytguard/internal/report"
	"ytguard/internal/store"
	"ytguard/internal/timekeeper"
)

const usage = `ytguard — family YouTube controls

Usage:
  sudo ytguard install [--admin-addr :8443] [--youtube-restrict 0|1|2] [--allow-extensions]
  sudo ytguard upgrade [--check] [--file PATH] [-y]
                                  install the latest release (or a local build)
  sudo ytguard uninstall [--purge]
  sudo ytguard passwd             reset the parent login (signs out all devices)
  sudo ytguard scan               look for other browsers / video apps now (runs hourly)
  ytguard serve [flags]           run the daemon (systemd does this)
  ytguard report --kid NAME [--day YYYY-MM-DD] [--send] [--html]
  ytguard policy                  print the Chrome policy JSON
  ytguard version

Most commands take --data DIR (default ` + install.DataDir + `).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = serve(args)
	case "install":
		err = cmdInstall(args)
	case "upgrade":
		fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
		var o install.UpgradeOptions
		fs.BoolVar(&o.Check, "check", false, "only check whether a newer release exists")
		fs.StringVar(&o.File, "file", "", "install this ytguard binary instead of downloading a release")
		fs.BoolVar(&o.Yes, "y", false, "don't ask for confirmation")
		fs.Parse(args)
		err = install.Upgrade(context.Background(), o)
	case "uninstall":
		fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
		purge := fs.Bool("purge", false, "also delete all data and the service user")
		fs.Parse(args)
		err = install.Uninstall(*purge)
	case "scan":
		fs := flag.NewFlagSet("scan", flag.ExitOnError)
		data := fs.String("data", install.DataDir, "data directory")
		quiet := fs.Bool("quiet", false, "don't print findings")
		fs.Parse(args)
		err = install.Scan(*data, !*quiet)
	case "passwd":
		err = passwd(args)
	case "report":
		err = cmdReport(args)
	case "policy":
		err = cmdPolicy(args)
	case "version", "--version", "-v":
		fmt.Println("ytguard", ytguard.Version)
		if len(args) > 0 && args[0] == "-v" {
			fmt.Println("repository:", firstNonEmpty(ytguard.Repo, "(none)"))
			fmt.Println("extension version:", ytguard.ExtensionVersion())
			fmt.Println("database schema:", store.SchemaVersion())
		}
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	var opt install.Options
	fs.StringVar(&opt.AdminAddr, "admin-addr", ":8443", "admin UI listen address")
	fs.IntVar(&opt.YouTubeRestrict, "youtube-restrict", 0, "also force YouTube Restricted Mode: 0 off, 1 moderate, 2 strict")
	fs.BoolVar(&opt.AllowExtensions, "allow-extensions", false, "don't block other Chrome extensions")
	fs.BoolVar(&opt.Upgrade, "upgrade", false, "reinstall this binary with the previous install's settings, without questions")
	fs.Parse(args)
	return install.Install(opt)
}

func openStore(fs *flag.FlagSet, args []string) (*store.Store, string, error) {
	data := fs.String("data", install.DataDir, "data directory")
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	st, err := store.Open(*data)
	return st, *data, err
}

func passwd(args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	st, dir, err := openStore(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	p := install.NewPrompter()
	cur, _, _ := st.Admin()
	u := p.Ask("Admin username", firstNonEmpty(cur, "parent"))
	pw := p.NewPassword()
	if err := auth.New(st).SetPassword(u, pw); err != nil {
		return err
	}
	st.Audit("cli", "local", "password reset", "all devices signed out")
	fmt.Println("Password changed. All browser sessions were signed out.")
	if p.YesNo("Also revoke all API tokens (e.g. Home Assistant)?", false) {
		if err := st.DeleteAllTokens(); err != nil {
			return err
		}
		fmt.Println("API tokens revoked.")
	}
	return install.FixOwnership(dir)
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	kid := fs.String("kid", "", "kid name or Linux user")
	day := fs.String("day", timekeeper.Day(time.Now()), "day (YYYY-MM-DD)")
	send := fs.Bool("send", false, "send through the configured channels")
	html := fs.Bool("html", false, "print HTML instead of text")
	st, dir, err := openStore(fs, args)
	if err != nil {
		return err
	}
	defer func() { st.Close(); install.FixOwnership(dir) }()
	k, err := st.KidByName(*kid)
	if err != nil {
		return fmt.Errorf("kid %q: %w", *kid, err)
	}
	r, err := report.Build(st, k, *day)
	if err != nil {
		return err
	}
	if *send {
		s, _ := st.Settings()
		if err := report.Send(context.Background(), s, r); err != nil {
			return err
		}
		fmt.Println("sent")
		return nil
	}
	if *html {
		h, err := r.HTML()
		fmt.Println(h)
		return err
	}
	fmt.Print(r.Text())
	return nil
}

func cmdPolicy(args []string) error {
	fs := flag.NewFlagSet("policy", flag.ExitOnError)
	data := fs.String("data", install.DataDir, "data directory")
	fs.Parse(args)
	key, err := install.ExtensionKey(*data)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(install.Policy(crx.ExtensionID(&key.PublicKey), install.Options{}, install.Blocklist()), "", "  ")
	fmt.Println(string(b))
	return nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data := fs.String("data", install.DataDir, "data directory")
	adminAddr := fs.String("admin-addr", ":8443", "admin UI listen address")
	adminHTTP := fs.Bool("admin-http", false, "serve the admin UI over plain HTTP (only on a loopback address, behind a TLS reverse proxy)")
	trustProxy := fs.Bool("trust-proxy", false, "use X-Forwarded-For for client IPs (with --admin-http behind a proxy)")
	dev := fs.Bool("dev", false, "development mode: accept unpacked extensions; allow --admin-http on any address")
	fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	st, err := store.Open(*data)
	if err != nil {
		return err
	}
	defer st.Close()
	key, err := install.ExtensionKey(*data)
	if err != nil {
		return err
	}
	app := core.New(st)
	au := auth.New(st)
	if _, _, err := st.Admin(); errors.Is(err, store.ErrNotFound) {
		slog.Warn("no parent login yet; run: sudo ytguard passwd --data " + *data)
	}

	ext := &extapi.Server{App: app, Key: key, Addr: install.ExtAddr, Dev: *dev}
	if err := ext.Prepare(); err != nil {
		return fmt.Errorf("pack extension: %w", err)
	}
	ui, err := admin.New(app, au)
	if err != nil {
		return err
	}
	ui.TrustProxy = *trustProxy

	extSrv := &http.Server{Addr: install.ExtAddr, Handler: ext.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	adminSrv := &http.Server{Addr: *adminAddr, Handler: ui.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}

	if *adminHTTP && !*dev && !isLoopback(*adminAddr) {
		return errors.New("--admin-http is only allowed on a loopback address (e.g. 127.0.0.1:8080) behind a TLS reverse proxy")
	}
	if !*adminHTTP {
		cert, err := install.TLSCert(*data)
		if err != nil {
			return fmt.Errorf("tls certificate: %w", err)
		}
		adminSrv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		slog.Info("admin certificate", "sha256", install.Fingerprint(cert))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 2)
	go func() {
		slog.Info("extension API listening", "addr", extSrv.Addr, "extension_id", ext.ExtensionID())
		errc <- extSrv.ListenAndServe()
	}()
	go func() {
		slog.Info("admin UI listening", "addr", adminSrv.Addr, "tls", !*adminHTTP)
		if *adminHTTP {
			errc <- adminSrv.ListenAndServe()
		} else {
			errc <- adminSrv.ListenAndServeTLS("", "")
		}
	}()
	sched := &report.Scheduler{St: st, Now: time.Now}
	go sched.Run(ctx)
	go app.Updates.Run(ctx)
	go app.RunAppMonitor(ctx, "/proc")
	slog.Info("ytguard starting", "version", ytguard.Version, "repo", ytguard.Repo, "schema", store.SchemaVersion())
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			st.PurgeExpiredSessions(time.Now().Unix())
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	slog.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	extSrv.Shutdown(sctx)
	adminSrv.Shutdown(sctx)
	return nil
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
