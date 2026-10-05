package console

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"ytguard/internal/install"
	"ytguard/internal/pairing"
)

// Phone access: a second listener, HTTPS on every network interface, so
// phones and other computers at home (or on a VPN) can use the console
// while it runs. The console's own address on this computer
// (http://127.0.0.1:8444) keeps working either way.

// DefaultPhonePort is where phone access listens unless changed.
const DefaultPhonePort = 8445

// Phone is the saved phone-access setting.
type Phone struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
}

const phoneKey = "console_phone"

// PhoneSetting returns the saved setting.
func (s *Server) PhoneSetting() Phone {
	p := Phone{Port: DefaultPhonePort}
	_, _ = s.St.GetJSON(phoneKey, &p)
	if p.Port <= 0 || p.Port > 65535 {
		p.Port = DefaultPhonePort
	}
	return p
}

// SetPhone saves the setting and applies it right away.
func (s *Server) SetPhone(p Phone) error {
	if p.Port < 1024 || p.Port > 65535 {
		return errors.New("pick a port between 1024 and 65535")
	}
	if err := s.St.SetJSON(phoneKey, p); err != nil {
		return err
	}
	return s.StartPhone()
}

// StartPhone starts, restarts or stops the phone listener to match the
// saved setting.
func (s *Server) StartPhone() error {
	p := s.PhoneSetting()
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.rsrv != nil && (!p.Enabled || p.Port != s.rport) {
		s.stopPhoneLocked()
	}
	s.rerr = ""
	if !p.Enabled || s.rsrv != nil {
		return nil
	}
	err := func() error {
		if s.DataDir == "" {
			return errors.New("no data directory for the certificate")
		}
		cert, err := install.TLSCert(s.DataDir)
		if err != nil {
			return fmt.Errorf("certificate: %w", err)
		}
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(p.Port))
		if err != nil {
			return fmt.Errorf("port %d is in use or blocked; pick another (%v)", p.Port, err)
		}
		srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second,
			WriteTimeout: 90 * time.Second, IdleTimeout: 120 * time.Second,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
		go func() {
			if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("phone access stopped", "err", err)
			}
		}()
		s.rsrv, s.rport, s.rfp = srv, p.Port, pairing.Fingerprint(cert.Certificate[0])
		slog.Info("phone access on", "port", p.Port, "certificate", pairing.Pretty(s.rfp))
		return nil
	}()
	if err != nil {
		s.rerr = err.Error()
	}
	return err
}

func (s *Server) stopPhoneLocked() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.rsrv.Shutdown(ctx)
	s.rsrv = nil
	slog.Info("phone access off")
}

// Close stops phone access.
func (s *Server) Close() {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.rsrv != nil {
		s.stopPhoneLocked()
	}
}

// PhoneStatus is shown on the Settings page.
type PhoneStatus struct {
	Phone
	Running     bool
	Error       string
	URLs        []string
	Fingerprint string
}

func (s *Server) phoneStatus() PhoneStatus {
	st := PhoneStatus{Phone: s.PhoneSetting()}
	s.rmu.Lock()
	st.Running, st.Error, st.Fingerprint = s.rsrv != nil, s.rerr, s.rfp
	port := s.rport
	s.rmu.Unlock()
	if st.Running {
		for _, ip := range lanAddrs() {
			st.URLs = append(st.URLs, "https://"+net.JoinHostPort(ip, strconv.Itoa(port)))
		}
	}
	return st
}

// virtualIface matches network adapters of containers and virtual
// machines, whose addresses a phone can't reach.
var virtualIface = []string{"docker", "br-", "virbr", "veth", "vmnet", "vboxnet", "virtualbox", "vmware",
	"lxc", "lxd", "cni", "flannel", "podman", "vethernet", "hyper-v"}

// lanAddrs lists this computer's home-network and VPN IPv4 addresses,
// the one it uses to reach the internet (normally its Wi-Fi or wired
// address) first.
func lanAddrs() []string {
	var primary string
	if c, err := net.Dial("udp", "192.0.2.1:9"); err == nil { // no packet is sent
		primary = c.LocalAddr().(*net.UDPAddr).IP.String()
		c.Close()
	}
	ifaces, _ := net.Interfaces()
	var out []string
	for _, ifc := range ifaces {
		name := strings.ToLower(ifc.Name)
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagRunning == 0 || ifc.Flags&net.FlagLoopback != 0 || slices.ContainsFunc(virtualIface, func(v string) bool { return strings.Contains(name, v) }) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil || n.IP.IsLinkLocalUnicast() || !homeNetwork(n.IP) {
				continue
			}
			out = append(out, n.IP.To4().String())
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i] == primary && out[j] != primary })
	return out
}

func (s *Server) settingsPhone(w http.ResponseWriter, r *req) {
	port, err := strconv.Atoi(r.FormValue("port"))
	if err != nil {
		port = DefaultPhonePort
	}
	on := r.FormValue("enabled") == "on"
	err = s.SetPhone(Phone{Enabled: on, Port: port})
	msg := "Phone access is off."
	if on {
		msg = "Phone access is on."
	}
	if err == nil {
		s.St.Audit("console", r.IP, "phone access", fmt.Sprintf("enabled=%v port=%d", on, port))
	}
	q := "msg=" + url.QueryEscape(msg)
	if err != nil {
		q = "err=" + url.QueryEscape(err.Error())
	}
	http.Redirect(w, r.Request, "/settings?"+q+"#phone", http.StatusSeeOther)
}
