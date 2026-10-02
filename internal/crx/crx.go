// Package crx packs and signs a Chrome extension as a CRX3 file, so the
// daemon can serve it to Chrome for policy force-install.
package crx

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// LoadOrCreateKey reads an RSA key from path, generating one if missing.
// The key fixes the extension ID, so keep it with the data directory.
func LoadOrCreateKey(p string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		der := x509.MarshalPKCS1PrivateKey(key)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: not PEM", p)
	}
	return x509.ParsePKCS1PrivateKey(blk.Bytes)
}

func crxID(pub *rsa.PublicKey) ([]byte, []byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(der)
	return sum[:16], der, nil
}

// ExtensionID returns the 32-letter Chrome extension ID for a key.
func ExtensionID(pub *rsa.PublicKey) string {
	id, _, err := crxID(pub)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range id {
		sb.WriteByte('a' + b>>4)
		sb.WriteByte('a' + b&0xf)
	}
	return sb.String()
}

// Zip builds a deterministic zip of the files under root in fsys. edit, if
// non-nil, may modify manifest.json before it's written.
func Zip(fsys fs.FS, root string, edit func(map[string]any)) ([]byte, error) {
	var names []string
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		data, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		rel := strings.TrimPrefix(n, strings.TrimSuffix(root, "/")+"/")
		if rel == "manifest.json" && edit != nil {
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("manifest.json: %w", err)
			}
			edit(m)
			if data, err = json.MarshalIndent(m, "", "  "); err != nil {
				return nil, err
			}
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: path.Clean(rel), Method: zip.Deflate})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Pack signs a zip archive into CRX3 format.
func Pack(zipData []byte, key *rsa.PrivateKey) ([]byte, error) {
	id, pubDER, err := crxID(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	signedData := protoBytes(1, id) // SignedData{crx_id}

	h := sha256.New()
	h.Write([]byte("CRX3 SignedData\x00"))
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(signedData)))
	h.Write(n[:])
	h.Write(signedData)
	h.Write(zipData)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h.Sum(nil))
	if err != nil {
		return nil, err
	}
	proof := append(protoBytes(1, pubDER), protoBytes(2, sig)...) // AsymmetricKeyProof
	header := append(protoBytes(2, proof), protoBytes(10000, signedData)...)

	var out bytes.Buffer
	out.WriteString("Cr24")
	binary.Write(&out, binary.LittleEndian, uint32(3))
	binary.Write(&out, binary.LittleEndian, uint32(len(header)))
	out.Write(header)
	out.Write(zipData)
	return out.Bytes(), nil
}

// protoBytes encodes a length-delimited protobuf field.
func protoBytes(field int, b []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(field)<<3|2)
	out = binary.AppendUvarint(out, uint64(len(b)))
	return append(out, b...)
}

// UpdateManifest returns the gupdate XML Chrome polls for updates.
func UpdateManifest(appID, codebase, version string) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<gupdate xmlns="http://www.google.com/update2/response" protocol="2.0">
  <app appid="%s">
    <updatecheck codebase="%s" version="%s" />
  </app>
</gupdate>
`, appID, codebase, version))
}
