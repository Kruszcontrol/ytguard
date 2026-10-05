package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"ytguard"
	"ytguard/internal/auth"
	"ytguard/internal/console"
	"ytguard/internal/install"
	"ytguard/internal/store"
)

const consoleUsage = `ytguard console — manage the kids on several YTGuard PCs from one page

Usage:
  ytguard console [--listen 127.0.0.1:8444] [--data DIR] [--open]
  ytguard console passwd [--data DIR]   reset the console login

The console answers on this computer at http://127.0.0.1:8444. To use it from
your phone too, turn on Settings → Phone access (HTTPS, home network or VPN
only).
`

func defaultConsoleDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "ytguard-console")
}

func cmdConsole(args []string) error {
	if len(args) > 0 && args[0] == "passwd" {
		return consolePasswd(args[1:])
	}
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Print(consoleUsage)
		return nil
	}
	fs := flag.NewFlagSet("console", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, consoleUsage); fs.PrintDefaults() }
	data := fs.String("data", defaultConsoleDir(), "data directory")
	listen := fs.String("listen", "127.0.0.1:8444", "address to listen on")
	open := fs.Bool("open", false, "open the console in the web browser")
	parseFlags(fs, args)
	return runConsole(*data, *listen, *open)
}

func runConsole(data, listen string, open bool) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	st, err := store.Open(data)
	if err != nil {
		return err
	}
	defer st.Close()
	srv, err := console.New(st, auth.New(st))
	if err != nil {
		return err
	}
	srv.DataDir = data
	defer srv.Close()
	tlsOn := !isLoopback(listen)
	hs := &http.Server{Addr: listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 60 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 120 * time.Second}
	scheme := "http"
	if tlsOn {
		cert, err := install.TLSCert(data)
		if err != nil {
			return fmt.Errorf("tls certificate: %w", err)
		}
		hs.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		scheme = "https"
		slog.Info("console certificate", "sha256", install.Fingerprint(cert))
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("can't listen on %s (is the console already running?): %w", listen, err)
	}
	url := scheme + "://" + browseAddr(ln.Addr().String())
	fmt.Printf("YTGuard console %s\nOpen %s in your browser. Keep this window open while you use it; press Ctrl+C to stop.\nData: %s\n", ytguard.Version, url, data)
	if err := srv.StartPhone(); err != nil {
		fmt.Println("Phone access couldn't start:", err)
	} else if p := srv.PhoneSetting(); p.Enabled {
		fmt.Printf("Phone access is on (port %d); see Settings → Phone access for the address.\n", p.Port)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		if tlsOn {
			errc <- hs.ServeTLS(ln, "", "")
		} else {
			errc <- hs.Serve(ln)
		}
	}()
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
	if open {
		openBrowser(url)
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}

// browseAddr turns a listen address into one a browser on this computer
// can open ("[::]:8444" → "127.0.0.1:8444").
func browseAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		c = exec.Command("open", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	if err := c.Start(); err != nil {
		slog.Warn("couldn't open the browser", "err", err)
	}
}

func consolePasswd(args []string) error {
	fs := flag.NewFlagSet("console passwd", flag.ExitOnError)
	data := fs.String("data", defaultConsoleDir(), "data directory")
	parseFlags(fs, args)
	st, err := store.Open(*data)
	if err != nil {
		return err
	}
	defer st.Close()
	p := install.NewPrompter()
	cur, _, _ := st.Admin()
	u := p.Ask("Console username", firstNonEmpty(cur, "parent"))
	pw := p.NewPassword()
	if err := auth.New(st).SetPassword(u, pw); err != nil {
		return err
	}
	st.Audit("cli", "local", "console password reset", "all devices signed out")
	fmt.Println("Console login changed. All browsers were signed out.")
	return nil
}
