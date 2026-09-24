package core

import "testing"

func TestBuiltInConnectionPolicy(t *testing.T) {
	p := defaultUserPolicy()
	if *p.BufferSize != 32 || *p.ConnectionIdle != 120 || *p.Handshake != 4 || *p.UplinkOnly != 2 || *p.DownlinkOnly != 4 {
		t.Fatal("unexpected built-in connection policy")
	}
	if _, err := p.Build(); err != nil {
		t.Fatal(err)
	}
}
