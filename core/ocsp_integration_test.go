package core

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	xtls "github.com/xtls/xray-core/transport/internet/tls"
	"golang.org/x/crypto/ocsp"
)

func TestOCSPRefreshAndCoreShutdown(t *testing.T) {
	requests := make(chan struct{}, 8)
	aborted := make(chan struct{}, 1)
	var response []byte
	responder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consume the POST body so the server can observe a cancelled client.
		io.Copy(io.Discard, r.Body)
		requests <- struct{}{}
		if r.URL.Path == "/blocked" {
			<-r.Context().Done()
			aborted <- struct{}{}
			return
		}
		w.Write(response)
	}))
	t.Cleanup(responder.Close)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	certFiles := protocolTestCertificate(t)
	writeCertificate := func(url string) ([]byte, []byte) {
		template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, OCSPServer: []string{url}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
		if err := os.WriteFile(certFiles.CertFile, certPEM, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(certFiles.KeyFile, keyPEM, 0600); err != nil {
			t.Fatal(err)
		}
		return certPEM, keyPEM
	}
	response, err = ocsp.CreateResponse(ca, ca, ocsp.Response{Status: ocsp.Good, SerialNumber: big.NewInt(2), ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(time.Hour)}, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := writeCertificate(responder.URL + "/response")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config := (&xtls.Config{Certificate: []*xtls.Certificate{{Certificate: certPEM, Key: keyPEM, OneTimeLoading: true, OcspStapling: 3600}}}).GetTLSConfigWithContext(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for {
		cert, err := config.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(cert.OCSPStaple, response) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("OCSP staple was not refreshed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-requests
	writeCertificate(responder.URL + "/blocked")
	vc := New(conf.New())
	if err := vc.Start(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vc.Close() })
	info := &panel.NodeInfo{Type: "vless", Security: panel.Tls, Common: &panel.CommonNode{ListenIP: "127.0.0.1", ServerPort: protocolFreePort(t), Network: "tcp", CertInfo: certFiles}}
	if err := vc.AddNode("ocsp-lifecycle", info); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("core did not start OCSP request")
	}
	if err := vc.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("core shutdown retained OCSP HTTP request")
	}
}
