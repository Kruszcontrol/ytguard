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

// Chrome rejects the whole ExtensionSettings policy if any value is
// unknown, so check every field against the values Chrome accepts.
func TestExtensionSettingsValid(t *testing.T) {
	modes := map[string]bool{"allowed": true, "blocked": true, "removed": true, "force_installed": true, "normal_installed": true}
	pins := map[string]bool{"force_pinned": true, "default_pinned": true, "default_unpinned": true}
	known := map[string]bool{"installation_mode": true, "update_url": true, "blocked_install_message": true, "toolbar_pin": true,
		"runtime_allowed_hosts": true, "runtime_blocked_hosts": true, "allowed_types": true, "install_sources": true,
		"blocked_permissions": true, "minimum_version_required": true, "override_update_url": true}
	for _, allow := range []bool{false, true} {
		es := Policy("abcdefghijklmnopabcdefghijklmnop", Options{AllowExtensions: allow}, nil)["ExtensionSettings"].(map[string]any)
		for id, v := range es {
			for k, val := range v.(map[string]any) {
				if !known[k] {
					t.Errorf("%s: unknown field %q", id, k)
				}
				if k == "installation_mode" && !modes[val.(string)] {
					t.Errorf("%s: bad installation_mode %q", id, val)
				}
				if k == "toolbar_pin" && !pins[val.(string)] {
					t.Errorf("%s: bad toolbar_pin %q", id, val)
				}
			}
		}
	}
}
