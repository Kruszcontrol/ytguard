// Package auth handles the parent's credentials: password hashing, browser
// sessions ("remember this device"), API tokens for integrations and login
// rate limiting. These are app credentials only; no OS account is involved.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"ytguard/internal/store"
)

const (
	pbkdf2Iter = 600_000
	// MinPasswordLen is the minimum admin password length.
	MinPasswordLen = 10

	ScopeRead    = "read"
	ScopeControl = "control"
	ScopeAdmin   = "admin"

	// ShortSession is how long a login lasts without "remember this device".
	ShortSession = 12 * time.Hour
)

// Scopes lists valid API token scopes.
var Scopes = []string{ScopeRead, ScopeControl, ScopeAdmin}

var (
	ErrBadLogin = errors.New("wrong username or password")
	ErrNoAdmin  = errors.New("no admin account; run 'sudo ytguard passwd'")
)

// LockedError means login is temporarily refused.
type LockedError struct{ Wait time.Duration }

func (e LockedError) Error() string {
	return fmt.Sprintf("too many failed logins; try again in %s", e.Wait.Round(time.Second))
}

// HashPassword returns a PBKDF2-HMAC-SHA256 hash string.
func HashPassword(pw string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	dk := pbkdf2([]byte(pw), salt, pbkdf2Iter, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(dk))
}

// CheckPassword verifies pw against a hash from HashPassword.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pbkdf2 implements RFC 8018 PBKDF2 with HMAC-SHA256.
func pbkdf2(pw, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, pw)
	hLen := prf.Size()
	var out []byte
	buf := make([]byte, 4)
	for block := 1; len(out) < keyLen; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0], buf[1], buf[2], buf[3] = byte(block>>24), byte(block>>16), byte(block>>8), byte(block)
		prf.Write(buf)
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := 0; j < hLen; j++ {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// ValidatePassword checks password strength rules.
func ValidatePassword(pw string) error {
	if len([]rune(pw)) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	return nil
}

// RandomToken returns a URL-safe random string with 256 bits of entropy.
func RandomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken hashes a session or API token for storage.
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// Auth ties credentials to the store.
type Auth struct {
	St  *store.Store
	Now func() time.Time
	lim *limiter
}

// New returns an Auth.
func New(st *store.Store) *Auth {
	return &Auth{St: st, Now: time.Now, lim: newLimiter()}
}

// Login checks credentials with rate limiting.
func (a *Auth) Login(user, pw, ip string) error {
	now := a.Now()
	if wait := a.lim.wait(ip, now); wait > 0 {
		return LockedError{wait}
	}
	u, hash, err := a.St.Admin()
	if errors.Is(err, store.ErrNotFound) {
		return ErrNoAdmin
	}
	if err != nil {
		return err
	}
	okUser := subtle.ConstantTimeCompare([]byte(strings.ToLower(u)), []byte(strings.ToLower(strings.TrimSpace(user)))) == 1
	okPw := CheckPassword(hash, pw) // always run to keep timing even
	if !okUser || !okPw {
		a.lim.fail(ip, now)
		return ErrBadLogin
	}
	a.lim.success(ip)
	return nil
}

// SetPassword changes admin credentials and revokes all sessions.
func (a *Auth) SetPassword(user, pw string) error {
	if strings.TrimSpace(user) == "" {
		return errors.New("username is empty")
	}
	if err := ValidatePassword(pw); err != nil {
		return err
	}
	if err := a.St.SetAdmin(strings.TrimSpace(user), HashPassword(pw)); err != nil {
		return err
	}
	return a.St.DeleteAllSessions()
}

// NewSession creates a session; remember makes it last sessionDays.
func (a *Auth) NewSession(name, ip, ua string, remember bool, sessionDays int) (token string, se store.Session, err error) {
	now := a.Now()
	token = RandomToken()
	exp := now.Add(ShortSession)
	if remember {
		if sessionDays <= 0 {
			sessionDays = 90
		}
		exp = now.AddDate(0, 0, sessionDays)
	}
	se = store.Session{CSRF: RandomToken(), Name: name, IP: ip, UserAgent: ua, Remember: remember,
		CreatedAt: now.Unix(), LastUsed: now.Unix(), ExpiresAt: exp.Unix()}
	err = a.St.CreateSession(HashToken(token), &se)
	return token, se, err
}

// Session validates a session cookie value.
func (a *Auth) Session(token, ip string) (store.Session, error) {
	if token == "" {
		return store.Session{}, store.ErrNotFound
	}
	now := a.Now().Unix()
	se, err := a.St.SessionByHash(HashToken(token), now)
	if err != nil {
		return se, err
	}
	if now-se.LastUsed > 60 || se.IP != ip {
		a.St.TouchSession(se.ID, ip, now)
	}
	return se, nil
}

// NewToken creates an API token and returns its only plaintext copy.
func (a *Auth) NewToken(name string, scopes []string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("token name is empty")
	}
	var clean []string
	for _, s := range scopes {
		for _, v := range Scopes {
			if s == v {
				clean = append(clean, s)
			}
		}
	}
	if len(clean) == 0 {
		return "", errors.New("pick at least one scope")
	}
	tok := "ytg_" + RandomToken()
	return tok, a.St.CreateToken(strings.TrimSpace(name), HashToken(tok), clean)
}

// Token validates a bearer token.
func (a *Auth) Token(tok, ip string) (store.APIToken, error) {
	now := a.Now()
	if wait := a.lim.wait(ip, now); wait > 0 {
		return store.APIToken{}, LockedError{wait}
	}
	if !strings.HasPrefix(tok, "ytg_") {
		a.lim.fail(ip, now)
		return store.APIToken{}, store.ErrNotFound
	}
	t, err := a.St.TokenByHash(HashToken(tok))
	if err != nil {
		a.lim.fail(ip, now)
	}
	return t, err
}

// limiter does per-IP exponential lockout plus a global brake.
type limiter struct {
	mu     sync.Mutex
	ips    map[string]*ipState
	global []time.Time
}

type ipState struct {
	fails int
	until time.Time
}

const (
	freeFails    = 5
	globalWindow = 10 * time.Minute
	globalMax    = 50
)

func newLimiter() *limiter { return &limiter{ips: map[string]*ipState{}} }

func (l *limiter) wait(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.ips[ip]; s != nil && now.Before(s.until) {
		return s.until.Sub(now)
	}
	cut := now.Add(-globalWindow)
	for len(l.global) > 0 && l.global[0].Before(cut) {
		l.global = l.global[1:]
	}
	if len(l.global) >= globalMax {
		return l.global[0].Add(globalWindow).Sub(now)
	}
	return 0
}

func (l *limiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.ips[ip]
	if s == nil {
		s = &ipState{}
		l.ips[ip] = s
	}
	s.fails++
	if s.fails >= freeFails {
		d := time.Minute << min(s.fails-freeFails, 6) // 1 min doubling to ~1 h
		s.until = now.Add(d)
	}
	l.global = append(l.global, now)
}

func (l *limiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ips, ip)
}
