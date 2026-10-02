package ytguard

import "testing"

func TestVersions(t *testing.T) {
	for _, tc := range []struct {
		v, ext string
	}{
		{"v1.2.3", "1.2.3"},
		{"v1.2.3-4-gabc1234", "1.2.3.4"},
		{"v1.2.3-4-gabc1234-dirty", "1.2.3.4"},
		{"1.0.0", "1.0.0"},
		{"dev", "0.0.1"},
		{"abc1234", "0.0.1"},
	} {
		old := Version
		Version = tc.v
		if got := ExtensionVersion(); got != tc.ext {
			t.Errorf("%s: ext %s want %s", tc.v, got, tc.ext)
		}
		Version = old
	}
	newer := [][2]string{{"v1.2.4", "v1.2.3"}, {"v1.10.0", "v1.9.9"}, {"v2.0.0", "v1.99.0"}, {"v1.2.3-1-gabc", "v1.2.3"}, {"v1.0.0", "dev"}}
	for _, p := range newer {
		if !Newer(p[0], p[1]) || Newer(p[1], p[0]) {
			t.Errorf("Newer(%s, %s) wrong", p[0], p[1])
		}
	}
	if Newer("v1.2.3", "v1.2.3") || Newer("dev", "v1.0.0") {
		t.Error("equal/dev should not be newer")
	}
}
