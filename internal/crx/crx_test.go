package crx

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestPack(t *testing.T) {
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "k.pem"))
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{
		"ext/manifest.json": {Data: []byte(`{"manifest_version":3,"name":"x","version":"0"}`)},
		"ext/a.js":          {Data: []byte("1")},
	}
	z, err := Zip(fsys, "ext", func(m map[string]any) { m["version"] = "1.2.3" })
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(z), int64(len(z)))
	if err != nil || len(zr.File) != 2 || zr.File[0].Name != "a.js" || zr.File[1].Name != "manifest.json" {
		t.Fatalf("zip: %v %v", err, zr)
	}
	c, err := Pack(z, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(c[:4]) != "Cr24" || binary.LittleEndian.Uint32(c[4:]) != 3 {
		t.Fatal("bad magic")
	}
	hl := binary.LittleEndian.Uint32(c[8:])
	if !bytes.Equal(c[12+hl:], z) {
		t.Fatal("zip not at end")
	}
	id := ExtensionID(&key.PublicKey)
	if len(id) != 32 {
		t.Fatalf("id %q", id)
	}
	for _, ch := range id {
		if ch < 'a' || ch > 'p' {
			t.Fatalf("id %q", id)
		}
	}
	// Same key file → same ID.
	key2, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "k2.pem"))
	if ExtensionID(&key2.PublicKey) == id {
		t.Fatal("different keys gave same id")
	}
}
