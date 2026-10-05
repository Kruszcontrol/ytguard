package pairing

import (
	"testing"
	"time"
)

func TestPairing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fp := Fingerprint([]byte("pc certificate"))
	var p Pending
	if err := p.Check(Proof("123456", fp), fp, now); err != ErrNoCode {
		t.Fatalf("no code: got %v", err)
	}
	code, _ := p.Start(now)
	if len(code) != 6 || NormalizeCode(FormatCode(code)) != code {
		t.Fatalf("code %q", code)
	}
	// A relay presenting another certificate gets a proof for that one.
	other := Fingerprint([]byte("attacker certificate"))
	if err := p.Check(Proof(code, other), fp, now); err != ErrWrongCode {
		t.Fatalf("other cert: got %v", err)
	}
	if err := p.Check(Proof(code, fp), fp, now); err != nil {
		t.Fatalf("right proof: %v", err)
	}
	if err := p.Check(Proof(code, fp), fp, now); err != ErrNoCode {
		t.Fatalf("code reused: %v", err)
	}

	// Expiry.
	code, _ = p.Start(now)
	if err := p.Check(Proof(code, fp), fp, now.Add(CodeTTL+time.Second)); err != ErrNoCode {
		t.Fatalf("expired: %v", err)
	}

	// Too many wrong tries cancel the code.
	code, _ = p.Start(now)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < MaxTries; i++ {
		if err := p.Check(Proof(wrong, fp), fp, now); err != ErrWrongCode {
			t.Fatalf("try %d: %v", i, err)
		}
	}
	if err := p.Check(Proof(code, fp), fp, now); err != ErrNoCode {
		t.Fatalf("after %d wrong tries the code should be gone: %v", MaxTries, err)
	}
	if NormalizeCode(" 12-34 56 ") != "123456" {
		t.Fatal("normalize")
	}
}
