package install

import (
	"strings"
	"testing"
)

func TestValidateAddr(t *testing.T) {
	for _, ok := range []string{":8443", "0.0.0.0:8443", "192.168.1.5:8443", "kidpc.local:443", "[::1]:8443"} {
		if err := ValidateAddr(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "8443", ":8443\nExecStartPre=+/bin/sh -c x", ":8443 --dev", "a b:1", ":port"} {
		if ValidateAddr(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPolicyBlocksJavascriptURLs(t *testing.T) {
	p := Policy("abc", Options{}, []string{"yewtu.be"})
	bl := p["URLBlocklist"].([]string)
	if len(bl) != 2 || bl[0] != "javascript://*" || bl[1] != "yewtu.be" {
		t.Fatalf("%v", bl)
	}
	if _, ok := p["ExtensionSettings"].(map[string]any)["*"]; !ok {
		t.Fatal("other extensions not blocked by default")
	}
	if !strings.Contains(Unit(":8443"), "--admin-addr :8443\n") {
		t.Fatal("unit")
	}
}
