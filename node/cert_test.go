package node

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestGeneratedSelfCertificateLoadsAndTruncates(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(key, make([]byte, 10000), 0600); err != nil {
		t.Fatal(err)
	}
	if err := generateSelfSslCertificate("localhost", cert, key); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
		t.Fatal("generated TLS key pair is unusable", err)
	}
	data, _ := os.ReadFile(key)
	if len(data) >= 10000 {
		t.Fatal("old key data was not truncated")
	}
	if _, err := (&User{}).DecodePrivate("invalid PEM"); err == nil {
		t.Fatal("invalid PEM accepted")
	}
}
