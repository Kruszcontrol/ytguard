package auth

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"ytguard/internal/store"
)

func TestPBKDF2Vector(t *testing.T) {
	// RFC 7914 section 11.
	got := hex.EncodeToString(pbkdf2([]byte("passwd"), []byte("salt"), 1, 64))
	want := "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc49ca9cccf179b645991664b39d77ef317c71b845b1e30bd509112041d3a19783"
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func TestPasswordHash(t *testing.T) {
	h := HashPassword("correct horse battery")
	if !CheckPassword(h, "correct horse battery") || CheckPassword(h, "wrong") || CheckPassword("garbage", "x") {
		t.Fatal("password check broken")
	}
}

func newAuth(t *testing.T) (*Auth, *time.Time) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := New(st)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	if err := a.SetPassword("parent", "longenough1"); err != nil {
		t.Fatal(err)
	}
	return a, &now
}

func TestLoginAndLockout(t *testing.T) {
	a, now := newAuth(t)
	if err := a.Login("Parent", "longenough1", "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < freeFails; i++ {
		if err := a.Login("parent", "nope", "1.2.3.4"); !errors.Is(err, ErrBadLogin) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	var le LockedError
	if err := a.Login("parent", "longenough1", "1.2.3.4"); !errors.As(err, &le) {
		t.Fatalf("want locked, got %v", err)
	}
	// Other IPs unaffected.
	if err := a.Login("parent", "longenough1", "5.6.7.8"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute)
	if err := a.Login("parent", "longenough1", "1.2.3.4"); err != nil {
		t.Fatalf("after lockout: %v", err)
	}
}

func TestSessions(t *testing.T) {
	a, now := newAuth(t)
	tok, _, err := a.NewSession("phone", "ip", "ua", true, 90)
	if err != nil {
		t.Fatal(err)
	}
	short, _, _ := a.NewSession("pc", "ip", "ua", false, 90)
	if _, err := a.Session(tok, "ip"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(24 * time.Hour)
	if _, err := a.Session(tok, "ip"); err != nil {
		t.Fatal("remembered session should survive a day")
	}
	if _, err := a.Session(short, "ip"); err == nil {
		t.Fatal("short session should expire")
	}
	// Password change revokes everything.
	if err := a.SetPassword("parent", "anotherlong1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Session(tok, "ip"); err == nil {
		t.Fatal("session should be revoked after password change")
	}
}

func TestTokens(t *testing.T) {
	a, _ := newAuth(t)
	tok, err := a.NewToken("Home Assistant", []string{ScopeRead, ScopeControl, "bogus"})
	if err != nil {
		t.Fatal(err)
	}
	at, err := a.Token(tok, "ip")
	if err != nil || !at.HasScope(ScopeControl) || at.HasScope(ScopeAdmin) || at.HasScope("bogus") {
		t.Fatalf("%+v %v", at, err)
	}
	if _, err := a.Token("ytg_wrong", "ip"); err == nil {
		t.Fatal("bad token accepted")
	}
	// A token is not a session.
	if _, err := a.Session(tok, "ip"); err == nil {
		t.Fatal("token accepted as session")
	}
	if err := ValidatePassword("abc"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := ValidatePassword("abcd"); err != nil {
		t.Fatal(err)
	}
}
