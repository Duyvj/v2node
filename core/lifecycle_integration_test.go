package core

import (
	"bytes"
	"runtime"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
)

func tlsBackgroundWorkers() int {
	return backgroundWorkers("tls.setupOcspTicker.func1")
}

func backgroundWorkers(function string) int {
	buffer := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buffer, true)
		if n < len(buffer) {
			return bytes.Count(buffer[:n], []byte(function))
		}
		buffer = make([]byte, len(buffer)*2)
	}
}

func TestAnyTLSReloadReleasesClientWorker(t *testing.T) {
	before := backgroundWorkers("anytls.(*Client).cleanupIdleSessions")
	info := &panel.NodeInfo{Type: "anytls", Common: &panel.CommonNode{Network: "tcp", ServerPort: 8080}}
	for i := 0; i < 8; i++ {
		client := protocolClient(t, info)
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if after := backgroundWorkers("anytls.(*Client).cleanupIdleSessions"); after > before {
		t.Fatalf("AnyTLS reload retained workers: before=%d after=%d", before, after)
	}
}

func TestTLSReloadReleasesWorkers(t *testing.T) {
	cert := protocolTestCertificate(t)
	before := tlsBackgroundWorkers()
	for i := 0; i < 8; i++ {
		cfg := conf.New()
		cfg.LogConfig.Level = "error"
		vc := New(cfg)
		if err := vc.Start(nil); err != nil {
			t.Fatal(err)
		}
		info := &panel.NodeInfo{Type: "vless", Security: panel.Tls, Common: &panel.CommonNode{ListenIP: "127.0.0.1", ServerPort: protocolFreePort(t), Network: "tcp", CertInfo: cert}}
		if err := vc.AddNode("reload-test", info); err != nil {
			vc.Close()
			t.Fatal(err)
		}
		if err := vc.Close(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if tlsBackgroundWorkers() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("TLS reload retained background workers: before=%d after=%d", before, tlsBackgroundWorkers())
}
