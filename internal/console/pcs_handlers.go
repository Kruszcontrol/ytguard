package console

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"ytguard/internal/pairing"
)

func (s *Server) pcsPage(w http.ResponseWriter, r *req) {
	snap := s.fetch(r.Context())
	s.page(w, r, "pcs", map[string]any{"Snap": snap})
}

var reFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

// pcProbe is step 1 of adding a PC: connect and show its certificate.
func (s *Server) pcProbe(w http.ResponseWriter, r *req) {
	addr, err := NormalizeAddr(r.FormValue("addr"))
	if err != nil {
		back(w, r, "/pcs", "", err)
		return
	}
	fp, err := Probe(r.Context(), addr)
	if err != nil {
		back(w, r, "/pcs", "", err)
		return
	}
	s.page(w, r, "pcadd", map[string]any{"Addr": addr, "Fingerprint": fp, "Error": ""})
}

// pcPair is step 2: pair with the code (or an API token) and save the PC
// with the certificate the parent saw in step 1.
func (s *Server) pcPair(w http.ResponseWriter, r *req) {
	addr, fp := r.FormValue("addr"), r.FormValue("fingerprint")
	code, token := pairing.NormalizeCode(r.FormValue("code")), strings.TrimSpace(r.FormValue("token"))
	name := truncate(strings.TrimSpace(r.FormValue("name")), 40)
	fail := func(err error) {
		w.WriteHeader(http.StatusBadRequest)
		s.page(w, r, "pcadd", map[string]any{"Addr": addr, "Fingerprint": fp, "Error": err.Error(), "Name": name})
	}
	if a, err := NormalizeAddr(addr); err != nil || a != addr || !reFingerprint.MatchString(fp) {
		back(w, r, "/pcs", "", errors.New("start again: enter the PC's address"))
		return
	}
	p := PC{Addr: addr, Fingerprint: fp}
	ctx, cancel := s.actx(r)
	defer cancel()
	switch {
	case token != "":
		p.Token = token
	case len(code) == 6:
		st, _ := s.St.Settings()
		var out struct {
			Token, PC string
			PCID      string `json:"pc_id"`
		}
		err := s.call(ctx, p, "POST", "/api/v1/pair", map[string]string{"proof": pairing.Proof(code, fp), "name": st.PCName}, &out)
		if err != nil {
			fail(err)
			return
		}
		p.Token = out.Token
	default:
		fail(errors.New("enter the 6-digit code shown on the PC"))
		return
	}
	// Check the token works and learn the PC's name.
	var st pcState
	if err := s.call(ctx, p, "GET", "/api/v1/state", nil, &st); err != nil {
		fail(err)
		return
	}
	p.PCID = st.PCID
	p.Name = name
	if p.Name == "" {
		p.Name = st.PC
	}
	if p.Name == "" {
		p.Name, _, _ = net.SplitHostPort(addr)
	}
	// Paired again: the old token is no longer needed.
	for _, o := range s.pcs() {
		if (o.PCID == p.PCID || o.Addr == p.Addr) && o.Token != p.Token {
			old := p
			old.ID, old.Token = "", o.Token
			_ = s.call(ctx, old, "POST", "/api/v1/token/revoke", nil, nil)
		}
	}
	if err := s.savePC(p); err != nil {
		fail(err)
		return
	}
	s.St.Audit("console", r.IP, "PC added", p.Name+" ("+addr+")")
	msg := "Connected to " + p.Name + "."
	if token != "" {
		var probe struct{}
		if err := s.call(ctx, p, "GET", "/api/v1/rules?type=video&q=-", nil, &probe); err != nil {
			msg += " Note: this token can't change filters (it needs the admin scope)."
		}
	}
	http.Redirect(w, r.Request, "/pcs?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) pcRename(w http.ResponseWriter, r *req) {
	p, err := s.pcFrom(r)
	if err == nil {
		name := truncate(strings.TrimSpace(r.FormValue("name")), 40)
		if name == "" {
			err = errors.New("enter a name")
		} else {
			p.Name = name
			err = s.savePC(p)
		}
	}
	back(w, r, "/pcs", "Renamed.", err)
}

// pcTrust re-pins a PC whose certificate changed, after the parent
// compared the new fingerprint with the PC's Security page.
func (s *Server) pcTrust(w http.ResponseWriter, r *req) {
	p, err := s.pcFrom(r)
	if err != nil {
		back(w, r, "/pcs", "", err)
		return
	}
	want := r.FormValue("fingerprint")
	got, err := Probe(r.Context(), p.Addr)
	if err != nil {
		back(w, r, "/pcs", "", err)
		return
	}
	if got != want {
		back(w, r, "/pcs", "", errors.New("the certificate changed again; compare it once more before trusting it"))
		return
	}
	p.Fingerprint = got
	if err := s.savePC(p); err != nil {
		back(w, r, "/pcs", "", err)
		return
	}
	s.St.Audit("console", r.IP, "PC certificate trusted", p.Name+" "+pairing.Short(got))
	back(w, r, "/pcs", "Trusting "+p.Name+"'s new certificate.", nil)
}

func (s *Server) pcRemove(w http.ResponseWriter, r *req) {
	p, err := s.pcFrom(r)
	if err != nil {
		back(w, r, "/pcs", "", err)
		return
	}
	// Tidy up the PC's side too, if it's reachable.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	revoked := s.call(ctx, p, "POST", "/api/v1/token/revoke", nil, nil) == nil
	if err := s.deletePC(p.ID); err != nil {
		back(w, r, "/pcs", "", err)
		return
	}
	s.St.Audit("console", r.IP, "PC removed", p.Name)
	msg := "Removed " + p.Name + " and revoked the console's token on it."
	if !revoked {
		msg = "Removed " + p.Name + ". It couldn't be reached, so revoke the console's token on its Security page."
	}
	back(w, r, "/pcs", msg, nil)
}
