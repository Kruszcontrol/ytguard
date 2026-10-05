// Package pairing connects a YTGuard console to a kid PC.
//
// The kid PC shows a short one-time code. The console never sends the code
// itself: it sends an HMAC of the certificate it sees, keyed with the code.
// The kid PC checks it against its own certificate, so a machine in the
// middle (presenting a different certificate) can't relay the pairing
// without first guessing the code. On success the PC returns an API token,
// and the console pins the certificate for every later connection.
package pairing

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

const (
	// CodeTTL is how long a pairing code works.
	CodeTTL = 10 * time.Minute
	// MaxTries is how many wrong answers cancel a code.
	MaxTries = 5
)

// NewCode returns a random 6-digit code.
func NewCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("%06d", n.Int64())
}

// FormatCode groups a code for display: "123 456".
func FormatCode(c string) string {
	if len(c) == 6 {
		return c[:3] + " " + c[3:]
	}
	return c
}

// NormalizeCode strips spaces and dashes a person may type.
func NormalizeCode(c string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, c)
}

// Fingerprint is the SHA-256 of a DER certificate, lowercase hex.
func Fingerprint(certDER []byte) string {
	sum := sha256.Sum256(certDER)
	return hex.EncodeToString(sum[:])
}

// Pretty formats a hex fingerprint as AB:CD:… groups.
func Pretty(fp string) string {
	fp = strings.ToUpper(fp)
	var parts []string
	for i := 0; i+2 <= len(fp); i += 2 {
		parts = append(parts, fp[i:i+2])
	}
	return strings.Join(parts, ":")
}

// Short is the first 8 bytes of a fingerprint, for comparing by eye.
func Short(fp string) string {
	if len(fp) > 16 {
		fp = fp[:16]
	}
	return Pretty(fp)
}

// Proof binds a code to a certificate fingerprint.
func Proof(code, fingerprint string) string {
	m := hmac.New(sha256.New, []byte(NormalizeCode(code)))
	m.Write([]byte("ytguard-pair-v1\n" + strings.ToLower(fingerprint)))
	return hex.EncodeToString(m.Sum(nil))
}

// Pending is the kid PC's current pairing code (at most one at a time).
type Pending struct {
	mu      sync.Mutex
	code    string
	expires time.Time
	tries   int
}

// Start creates a new code, replacing any previous one.
func (p *Pending) Start(now time.Time) (code string, expires time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.code, p.expires, p.tries = NewCode(), now.Add(CodeTTL), 0
	return p.code, p.expires
}

// Cancel forgets the current code.
func (p *Pending) Cancel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.code = ""
}

// Active returns the current code, if any.
func (p *Pending) Active(now time.Time) (code string, expires time.Time, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code == "" || now.After(p.expires) {
		return "", time.Time{}, false
	}
	return p.code, p.expires, true
}

// Errors from Check.
var (
	ErrNoCode    = fmt.Errorf("no pairing code is active on this PC; start one under Security")
	ErrWrongCode = fmt.Errorf("wrong pairing code")
)

// Check verifies a proof against this PC's certificate. A correct proof
// uses up the code; too many wrong ones cancel it.
func (p *Pending) Check(proof, fingerprint string, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code == "" || now.After(p.expires) {
		return ErrNoCode
	}
	want := Proof(p.code, fingerprint)
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(strings.TrimSpace(proof)))) {
		p.tries++
		if p.tries >= MaxTries {
			p.code = ""
		}
		return ErrWrongCode
	}
	p.code = ""
	return nil
}
