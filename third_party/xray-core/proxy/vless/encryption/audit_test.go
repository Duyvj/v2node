package encryption

import (
	"crypto/ecdh"
	"crypto/rand"
	"testing"
	"time"
)

func TestAuditCloseJoinsTicketWorker(t *testing.T) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := &ServerInstance{}
	if err := server.Init([][]byte{key.Bytes()}, 0, 600, 600, ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { server.Close(); server.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ticket worker retained until the next minute")
	}
}
