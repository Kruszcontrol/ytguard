package console

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"ytguard/internal/pairing"
)

// PC is a kid PC the console manages.
type PC struct {
	ID          string `json:"id"` // local ID, used in console URLs
	Name        string `json:"name"`
	Addr        string `json:"addr"`        // host:port of the PC's admin UI
	Fingerprint string `json:"fingerprint"` // pinned TLS certificate SHA-256 (hex)
	Token       string `json:"token"`       // API token the PC gave us
	PCID        string `json:"pc_id"`       // the PC's own ID (same as in Home Assistant)
	AddedAt     int64  `json:"added_at"`
}

// URL is the PC's admin UI address.
func (p PC) URL() string { return "https://" + p.Addr }

const pcsKey = "console_pcs"

// pcs loads the PC list, sorted by name.
func (s *Server) pcs() []PC {
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []PC
	_, _ = s.St.GetJSON(pcsKey, &list)
	sort.SliceStable(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	return list
}

func (s *Server) pc(id string) (PC, bool) {
	for _, p := range s.pcs() {
		if p.ID == id {
			return p, true
		}
	}
	return PC{}, false
}

// savePC adds or replaces a PC. A PC that's paired again (same PC ID or
// address) replaces its old entry instead of appearing twice.
func (s *Server) savePC(p PC) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []PC
	_, _ = s.St.GetJSON(pcsKey, &list)
	out := list[:0]
	for _, o := range list {
		same := o.ID == p.ID || (p.PCID != "" && o.PCID == p.PCID) || o.Addr == p.Addr
		if same {
			if p.ID == "" {
				p.ID = o.ID
			}
			continue
		}
		out = append(out, o)
	}
	if p.ID == "" {
		p.ID = newID()
	}
	if p.AddedAt == 0 {
		p.AddedAt = time.Now().Unix()
	}
	s.forget(p.ID)
	return s.St.SetJSON(pcsKey, append(out, p))
}

func (s *Server) deletePC(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []PC
	_, _ = s.St.GetJSON(pcsKey, &list)
	out := list[:0]
	for _, o := range list {
		if o.ID != id {
			out = append(out, o)
		}
	}
	s.forget(id)
	return s.St.SetJSON(pcsKey, out)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NormalizeAddr turns what a parent types ("192.168.1.20",
// "https://kids-pc.local:8443/", "kids-pc") into host:port.
func NormalizeAddr(in string) (string, error) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", errors.New("enter the PC's address, e.g. 192.168.1.20")
	}
	if strings.HasPrefix(strings.ToLower(s), "http://") {
		return "", errors.New("YTGuard PCs use https://, not http://")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("%q isn't a valid address", in)
	}
	port := u.Port()
	if port == "" {
		port = "8443"
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// ---- pinned HTTPS client ----

// CertChangedError means a PC presented a different certificate than the
// one pinned when it was added.
type CertChangedError struct{ Got string }

func (e CertChangedError) Error() string {
	return "this PC's certificate changed (now " + pairing.Short(e.Got) + "); check it before trusting it again"
}

// pinnedTLS accepts exactly the certificate with the given fingerprint.
// The kid PCs use self-signed certificates, so this replaces the usual
// certificate-authority check.
func pinnedTLS(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // verified by VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			if got := pairing.Fingerprint(cs.PeerCertificates[0].Raw); got != fingerprint {
				return CertChangedError{Got: got}
			}
			return nil
		},
	}
}

func (s *Server) client(p PC) *http.Client {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	key := p.ID + "/" + p.Fingerprint
	if c, ok := s.clients[key]; ok {
		return c
	}
	tr := &http.Transport{TLSClientConfig: pinnedTLS(p.Fingerprint), Proxy: nil,
		DialContext: (&net.Dialer{Timeout: 4 * time.Second}).DialContext, TLSHandshakeTimeout: 4 * time.Second,
		MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}
	c := &http.Client{Transport: tr, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	s.clients[key] = c
	return c
}

// forget drops cached connections for a PC (after its pin or token changed).
func (s *Server) forget(id string) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	for k, c := range s.clients {
		if strings.HasPrefix(k, id+"/") {
			c.CloseIdleConnections()
			delete(s.clients, k)
		}
	}
}

// Probe connects to a PC and returns its certificate fingerprint without
// trusting it yet.
func Probe(ctx context.Context, addr string) (string, error) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}} // only reading the certificate
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("couldn't reach %s over HTTPS: %w", addr, err)
	}
	defer c.Close()
	certs := c.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("the PC sent no certificate")
	}
	return pairing.Fingerprint(certs[0].Raw), nil
}

// ---- API calls ----

// APIError is an error reply from a PC.
type APIError struct {
	Status int
	Msg    string
}

func (e APIError) Error() string { return e.Msg }

// ErrTooOld means the PC's YTGuard doesn't have an API call yet.
var ErrTooOld = errors.New("this PC runs an older YTGuard without this feature; upgrade it with 'sudo ytguard upgrade'")

// call makes an API request to a PC. in (if not nil) is sent as JSON; out
// (if not nil) receives the JSON reply.
func (s *Server) call(ctx context.Context, p PC, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.URL()+path, body)
	if err != nil {
		return err
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client(p).Do(req)
	if err != nil {
		var cc CertChangedError
		if errors.As(err, &cc) {
			return cc
		}
		return fmt.Errorf("can't reach %s: %w", firstNonEmpty(p.Name, p.Addr), unwrapNet(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
				return ErrTooOld
			}
			e.Error = resp.Status
		}
		if resp.StatusCode == http.StatusUnauthorized {
			e.Error = "the console's access to this PC was revoked; pair it again"
		}
		return APIError{resp.StatusCode, e.Error}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// unwrapNet shortens Go's long URL errors to the useful part.
func unwrapNet(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("no answer (PC off, asleep or not on this network?)")
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return errors.New("connection refused or unreachable (PC off, or YTGuard not running?)")
	}
	return err
}

// forEach runs fn for every item (usually one per PC) in parallel and
// waits for all of them.
func forEach[I, T any](items []I, fn func(I) T) []T {
	out := make([]T, len(items))
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		go func(i int, it I) {
			defer wg.Done()
			out[i] = fn(it)
		}(i, it)
	}
	wg.Wait()
	return out
}
