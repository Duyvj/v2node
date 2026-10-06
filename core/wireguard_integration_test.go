package core

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/xtls/xray-core/common/session"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

func TestWireGuardHandshakeAndRoundTrip(t *testing.T) {
	serverKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tun, stack, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.77.0.1")}, nil, 1280)
	if err != nil {
		t.Fatal(err)
	}
	wg := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "test-wg: "))
	t.Cleanup(wg.Close)
	port := protocolFreePort(t)
	ipc := fmt.Sprintf("private_key=%s\nlisten_port=%d\npublic_key=%s\nallowed_ip=10.77.0.2/32\n", hex.EncodeToString(serverKey.Bytes()), port, hex.EncodeToString(clientKey.PublicKey().Bytes()))
	if err := wg.IpcSet(ipc); err != nil {
		t.Fatal(err)
	}
	if err := wg.Up(); err != nil {
		t.Fatal(err)
	}
	tcp, err := stack.ListenTCP(&net.TCPAddr{IP: net.ParseIP("10.77.0.1"), Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcp.Close() })
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	udp, err := stack.ListenUDP(&net.UDPAddr{IP: net.ParseIP("10.77.0.1"), Port: 8081})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udp.Close() })
	go func() {
		b := make([]byte, 2048)
		for {
			n, addr, err := udp.ReadFrom(b)
			if err != nil {
				return
			}
			udp.WriteTo(b[:n], addr)
		}
	}()
	out, err := json.Marshal(map[string]any{"tag": "wg-test-out", "protocol": "wireguard", "settings": map[string]any{
		"secretKey": base64.StdEncoding.EncodeToString(clientKey.Bytes()), "address": []string{"10.77.0.2/32"},
		"noKernelTun": true, "domainStrategy": "ForceIPv4", "mtu": 1280, "workers": 1,
		"peers": []any{map[string]any{"publicKey": base64.StdEncoding.EncodeToString(serverKey.PublicKey().Bytes()), "endpoint": fmt.Sprintf("127.0.0.1:%d", port), "allowedIPs": []string{"10.77.0.1/32"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	action := string(out)
	info := &panel.NodeInfo{Tag: "wg-test", Common: &panel.CommonNode{Routes: []panel.Route{{Action: "default_out", ActionValue: &action}}}}
	cfg := conf.New()
	cfg.LogConfig.Level = "error"
	vc := New(cfg)
	if err := vc.Start([]*panel.NodeInfo{info}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vc.Close() })
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			address := "10.77.0.1:8080"
			if network == "udp" {
				address = "10.77.0.1:8081"
			}
			ctx := session.ContextWithInbound(context.Background(), &session.Inbound{Tag: info.Tag})
			protocolExchangeContext(t, ctx, vc.Server, network, address, bytes.Repeat([]byte("wireguard-echo"), 16))
		})
	}
	state, err := wg.IpcGet()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(state, "last_handshake_time_sec=0\n") || !strings.Contains(state, "last_handshake_time_sec=") {
		t.Fatal("WireGuard handshake missing")
	}
}
