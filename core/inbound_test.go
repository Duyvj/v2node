package core

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/app/proxyman"
	xtls "github.com/xtls/xray-core/transport/internet/tls"
)

func writeTestCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "node.test"},
		DNSNames:     []string{"node.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "node.cer"), filepath.Join(dir, "node.key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile
}

func TestTLSInboundLoadsCertificateOnce(t *testing.T) {
	certFile, keyFile := writeTestCert(t)
	info := &panel.NodeInfo{
		Type:     "trojan",
		Security: panel.Tls,
		Common: &panel.CommonNode{
			Protocol:   "trojan",
			ListenIP:   "0.0.0.0",
			ServerPort: 443,
			Network:    "tcp",
			CertInfo:   &panel.CertInfo{CertMode: "file", CertFile: certFile, KeyFile: keyFile},
		},
	}
	cfg, err := buildInbound(info, "tag")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := cfg.ReceiverSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	var tlsConfig *xtls.Config
	for _, s := range inst.(*proxyman.ReceiverConfig).StreamSettings.SecuritySettings {
		if v, err := s.GetInstance(); err == nil {
			if c, ok := v.(*xtls.Config); ok {
				tlsConfig = c
			}
		}
	}
	if tlsConfig == nil || len(tlsConfig.Certificate) != 1 {
		t.Fatalf("tls config = %v", tlsConfig)
	}
	if cert := tlsConfig.Certificate[0]; !cert.OneTimeLoading || cert.OcspStapling != 0 {
		t.Fatalf("certificate oneTimeLoading=%v ocspStapling=%d: Xray starts a reload ticker that never stops when the inbound is removed", cert.OneTimeLoading, cert.OcspStapling)
	}
}
