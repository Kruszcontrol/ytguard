// Package install sets ytguard up on a PC: system user, data directory,
// keys and certificates, Chrome policy and the systemd service.
package install

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ytguard/internal/crx"
)

// Paths inside the data directory.
const (
	ExtKeyFile  = "extension.pem"
	CertFile    = "tls-cert.pem"
	CertKeyFile = "tls-key.pem"
)

// ExtensionKey loads or creates the extension signing key.
func ExtensionKey(dataDir string) (*rsa.PrivateKey, error) {
	return crx.LoadOrCreateKey(filepath.Join(dataDir, ExtKeyFile))
}

// TLSCert loads or creates the admin UI's self-signed certificate.
func TLSCert(dataDir string) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dataDir, CertFile), filepath.Join(dataDir, CertKeyFile)
	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		if err := GenerateCert(certPath, keyPath); err != nil {
			return tls.Certificate{}, err
		}
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

// GenerateCert writes a self-signed certificate for this host's names and IPs.
func GenerateCert(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "YTGuard " + host, Organization: []string{"YTGuard"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if host != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, host, host+".local", host+".lan", host+".home")
	}
	tmpl.IPAddresses = append(tmpl.IPAddresses, LANAddrs()...)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// LANAddrs lists this machine's non-loopback IP addresses.
func LANAddrs() []net.IP {
	var out []net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			out = append(out, n.IP)
		}
	}
	return out
}

// Fingerprint returns the SHA-256 fingerprint of a certificate, colon separated.
func Fingerprint(c tls.Certificate) string {
	if len(c.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(c.Certificate[0])
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}
