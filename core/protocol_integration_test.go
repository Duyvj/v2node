package core

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/limiter"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/uuid"
	xcore "github.com/xtls/xray-core/core"
	xconf "github.com/xtls/xray-core/infra/conf"
)

const protocolTestUUID = "11111111-1111-4111-8111-111111111111"

// These tests use the actual v2node inbound, user manager, dispatcher and
// limiter, with an Xray client and real TCP/UDP sockets on loopback.
func TestProtocolRoundTrip(t *testing.T) {
	cert := protocolTestCertificate(t)
	tcp, udp := protocolEchoServers(t)
	cases := []struct{ protocol, network, cipher string }{}
	for _, protocol := range []string{"vless", "vmess", "trojan", "anytls"} {
		for _, network := range []string{"tcp", "ws", "grpc", "httpupgrade", "xhttp"} {
			cases = append(cases, struct{ protocol, network, cipher string }{protocol, network, ""})
		}
	}
	for _, cipher := range []string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "xchacha20-ietf-poly1305", "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm"} {
		cases = append(cases, struct{ protocol, network, cipher string }{"shadowsocks", "tcp", cipher})
	}
	cases = append(cases, struct{ protocol, network, cipher string }{"hysteria2", "hysteria", ""}, struct{ protocol, network, cipher string }{"tuic", "tuic", ""})
	for _, network := range []string{"xhttp-stream-one", "xhttp-stream-up", "tcp-mlkem"} {
		cases = append(cases, struct{ protocol, network, cipher string }{"vless", network, ""})
	}
	cases = append(cases, struct{ protocol, network, cipher string }{"hysteria2", "hysteria-salamander", ""})
	for _, tc := range cases {
		name := tc.protocol + "/" + tc.network + tc.cipher
		t.Run(name, func(t *testing.T) {
			info := &panel.NodeInfo{Type: tc.protocol, Tag: name, Common: &panel.CommonNode{
				ListenIP: "127.0.0.1", ServerPort: protocolFreePort(t), Network: tc.network,
				NetworkSettings: protocolNetworkSettings(tc.network), Cipher: tc.cipher, CertInfo: cert,
			}}
			switch tc.network {
			case "xhttp-stream-one", "xhttp-stream-up":
				info.Common.Network = "xhttp"
				info.Common.NetworkSettings = json.RawMessage(fmt.Sprintf(`{"path":"/test","mode":%q}`, tc.network[6:]))
			case "hysteria-salamander":
				info.Common.Network = "hysteria"
				info.Common.Obfs = "salamander"
				info.Common.ObfsPassword = `quote"and\backslash-test`
			case "tcp-mlkem":
				info.Common.Network = "tcp"
				key, err := ecdh.X25519().GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				info.Common.Encryption = "mlkem768x25519plus"
				pqKey, err := mlkem.GenerateKey768()
				if err != nil {
					t.Fatal(err)
				}
				info.Common.EncryptionSettings = panel.EncSettings{Mode: "native", Ticket: "600s", PrivateKey: base64.RawURLEncoding.EncodeToString(pqKey.Bytes()) + "." + base64.RawURLEncoding.EncodeToString(key.Bytes())}
			}
			if tc.protocol != "shadowsocks" {
				info.Security = panel.Tls
			}
			if tc.protocol == "hysteria2" || tc.protocol == "tuic" {
				info.Common.NetworkSettings = nil
			}
			if tc.cipher == "2022-blake3-aes-128-gcm" {
				info.Common.ServerKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16))
			} else if tc.cipher == "2022-blake3-aes-256-gcm" {
				info.Common.ServerKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
			}
			cfg := conf.New()
			cfg.LogConfig.Level = "error"
			// Xray blocks private targets by default for public proxy inbounds.
			// Explicitly permit only the test echo server on loopback.
			out := `{"tag":"test-echo","protocol":"freedom","settings":{"finalRules":[{"action":"allow","ip":["127.0.0.1/32"]}]}}`
			info.Common.Routes = []panel.Route{{Action: "default_out", ActionValue: &out}}
			server := New(cfg)
			if err := server.Start([]*panel.NodeInfo{info}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { server.Close(); limiter.DeleteLimiter(info.Tag) })
			users := []panel.UserInfo{{Id: 1, Uuid: protocolTestUUID, DeviceLimit: 2}}
			limiter.AddLimiter(tc.protocol, info.Tag, users, nil)
			if err := server.AddNode(info.Tag, info); err != nil {
				t.Fatal(err)
			}
			if _, err := server.AddUsers(&AddUsersParams{Tag: info.Tag, Users: users, NodeInfo: info}); err != nil {
				t.Fatal(err)
			}
			if tc.protocol == "tuic" {
				protocolTUICRoundTrip(t, info, tcp, udp)
			} else {
				client := protocolClient(t, info)
				for _, network := range []string{"tcp", "udp"} {
					t.Run(network, func(t *testing.T) {
						if tc.protocol == "anytls" && network == "udp" {
							protocolAnyTLSUDP(t, info, udp)
						}
						address := tcp
						if network == "udp" {
							address = udp
						}
						payload := bytes.Repeat([]byte("v2node-test-payload!"), 32)
						if network == "tcp" {
							payload = bytes.Repeat(payload, 256)
						}
						protocolExchange(t, client, network, address, payload)
					})
				}
			}
			traffic, _ := server.SnapshotUserTraffic(info.Tag, 0)
			if len(traffic) != 1 || traffic[0].Upload == 0 || traffic[0].Download == 0 {
				t.Fatalf("missing traffic accounting: %+v", traffic)
			}
			// Removal must also close live sessions and clear per-user state.
			if err := server.DelUsers(users, info.Tag, info); err != nil {
				t.Fatal(err)
			}
			l, _ := limiter.GetLimiter(info.Tag)
			l.UpdateUser(info.Tag, nil, users, nil)
			if _, ok := server.dispatcher.LinkManagers.Load(info.Tag + "|" + protocolTestUUID); ok {
				t.Fatal("deleted user retained link manager")
			}
		})
	}
}

func protocolNetworkSettings(network string) json.RawMessage {
	switch network {
	case "ws", "httpupgrade":
		return json.RawMessage(`{"path":"/test"}`)
	case "grpc":
		return json.RawMessage(`{"serviceName":"test"}`)
	case "xhttp":
		return json.RawMessage(`{"path":"/test","mode":"packet-up"}`)
	default:
		return json.RawMessage(`{}`)
	}
}

func protocolClient(t *testing.T, info *panel.NodeInfo) *xcore.Instance {
	t.Helper()
	cm := info.Common
	stream := map[string]any{"network": cm.Network}
	switch cm.Network {
	case "ws":
		stream["wsSettings"] = json.RawMessage(cm.NetworkSettings)
	case "grpc":
		stream["grpcSettings"] = json.RawMessage(cm.NetworkSettings)
	case "httpupgrade":
		stream["httpupgradeSettings"] = json.RawMessage(cm.NetworkSettings)
	case "xhttp":
		stream["xhttpSettings"] = json.RawMessage(cm.NetworkSettings)
	case "hysteria":
		stream["hysteriaSettings"] = map[string]any{"version": 2, "auth": protocolTestUUID}
	}
	if cm.Obfs != "" {
		stream["finalmask"] = map[string]any{"udp": []any{map[string]any{"type": cm.Obfs, "settings": map[string]any{"password": cm.ObfsPassword}}}}
	}
	if info.Security == panel.Tls {
		stream["security"] = "tls"
		certPEM, err := os.ReadFile(cm.CertInfo.CertFile)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(certPEM)
		pin := sha256.Sum256(block.Bytes)
		tls := map[string]any{"serverName": "localhost", "pinnedPeerCertSha256": hex.EncodeToString(pin[:])}
		if info.Type == "hysteria2" || info.Type == "tuic" {
			tls["alpn"] = []string{"h3"}
		}
		stream["tlsSettings"] = tls
	}
	if info.Security == panel.Reality {
		secret, err := base64.RawURLEncoding.DecodeString(cm.TlsSettings.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ecdh.X25519().NewPrivateKey(secret)
		if err != nil {
			t.Fatal(err)
		}
		stream["security"] = "reality"
		stream["realitySettings"] = map[string]any{"serverName": "localhost", "fingerprint": "chrome", "publicKey": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), "shortId": cm.TlsSettings.ShortId}
	}
	endpoint := map[string]any{"address": "127.0.0.1", "port": cm.ServerPort}
	protocol := info.Type
	var settings any
	switch info.Type {
	case "vless", "vmess":
		account := map[string]any{"id": protocolTestUUID, "encryption": "none", "security": "auto"}
		if cm.Encryption == "mlkem768x25519plus" {
			var publicKeys []string
			for _, encoded := range strings.Split(cm.EncryptionSettings.PrivateKey, ".") {
				secret, err := base64.RawURLEncoding.DecodeString(encoded)
				if err != nil {
					t.Fatal(err)
				}
				var public []byte
				if len(secret) == 64 {
					key, err := mlkem.NewDecapsulationKey768(secret)
					if err != nil {
						t.Fatal(err)
					}
					public = key.EncapsulationKey().Bytes()
				} else {
					key, err := ecdh.X25519().NewPrivateKey(secret)
					if err != nil {
						t.Fatal(err)
					}
					public = key.PublicKey().Bytes()
				}
				publicKeys = append(publicKeys, base64.RawURLEncoding.EncodeToString(public))
			}
			account["encryption"] = cm.Encryption + "." + cm.EncryptionSettings.Mode + ".1rtt." + strings.Join(publicKeys, ".")
		}
		if cm.Flow != "" {
			account["flow"] = cm.Flow
		}
		endpoint["users"] = []any{account}
		settings = map[string]any{"vnext": []any{endpoint}}
	case "trojan":
		endpoint["password"] = protocolTestUUID
		settings = map[string]any{"servers": []any{endpoint}}
	case "shadowsocks":
		endpoint["method"] = cm.Cipher
		password := protocolTestUUID
		if cm.ServerKey != "" {
			size := 32
			if cm.Cipher == "2022-blake3-aes-128-gcm" {
				size = 16
			}
			password = cm.ServerKey + ":" + base64.StdEncoding.EncodeToString([]byte(protocolTestUUID[:size]))
		}
		endpoint["password"] = password
		settings = map[string]any{"servers": []any{endpoint}}
	case "anytls", "tuic":
		endpoint["password"] = protocolTestUUID
		endpoint["id"] = protocolTestUUID
		settings = endpoint
	case "hysteria2":
		protocol = "hysteria"
		endpoint["version"] = 2
		settings = endpoint
	}
	if info.Type == "raw-test" {
		protocol = "freedom"
		settings = map[string]any{"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}
	}
	raw, err := json.Marshal(map[string]any{"log": map[string]any{"loglevel": "error", "access": "none"}, "outbounds": []any{map[string]any{"protocol": protocol, "settings": settings, "streamSettings": stream}}})
	if err != nil {
		t.Fatal(err)
	}
	var config xconf.Config
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	built, err := config.Build()
	if err != nil {
		t.Fatal(err)
	}
	client, err := xcore.New(built)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	return client
}

func protocolExchange(t *testing.T, client *xcore.Instance, network, address string, payload []byte) {
	protocolExchangeContext(t, context.Background(), client, network, address, payload)
}

func protocolExchangeContext(t *testing.T, parent context.Context, client *xcore.Instance, network, address string, payload []byte) {
	t.Helper()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := xnet.ParseDestination(network + ":" + net.JoinHostPort(host, port))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	conn, err := xcore.Dial(ctx, client, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan error, 1)
	go func() {
		if network == "tcp" {
			// Complete the handshake before queuing a bulk upload. The pinned
			// Trojan outbound buffers its first payload together with the header.
			if _, err := conn.Write(payload[:1]); err != nil {
				done <- err
				return
			}
			var first [1]byte
			if _, err := io.ReadFull(conn, first[:]); err != nil {
				done <- err
				return
			}
			if first[0] != payload[0] {
				done <- fmt.Errorf("first byte mismatch")
				return
			}
			payload = payload[1:]
		}
		writes := make(chan error, 1)
		go func() {
			// Feed ordinary client-sized chunks, including the first payload.
			// A single giant core.Dial write can overflow a protocol header buffer.
			for remaining := payload; len(remaining) > 0; {
				n, err := conn.Write(remaining[:min(len(remaining), 4096)])
				if err != nil {
					writes <- err
					return
				}
				remaining = remaining[n:]
			}
			writes <- nil
		}()
		received := make([]byte, len(payload))
		_, readErr := io.ReadFull(conn, received)
		if readErr != nil {
			done <- readErr
			return
		}
		if err := <-writes; err != nil {
			done <- err
			return
		}
		if !bytes.Equal(payload, received) {
			done <- fmt.Errorf("payload mismatch")
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		cancel()
		conn.Close()
		t.Fatal("round trip timed out")
	}
}

// Exercise the server's AnyTLS UDP-over-TCP path with an independent wire
// client as well as the patched Xray client used in the main protocol matrix.
func protocolAnyTLSUDP(t *testing.T, info *panel.NodeInfo, destination string) {
	t.Helper()
	rawInfo := *info
	rawInfo.Type = "raw-test"
	client := protocolClient(t, &rawInfo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := xcore.Dial(ctx, client, xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(info.Common.ServerPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	timer := time.AfterFunc(8*time.Second, func() { cancel(); conn.Close() })
	defer timer.Stop()
	hash := sha256.Sum256([]byte(protocolTestUUID))
	if _, err := conn.Write(append(hash[:], 0, 0)); err != nil {
		t.Fatal(err)
	}
	send := func(command byte, body []byte) {
		header := []byte{command, 0, 0, 0, 1}
		header = binary.BigEndian.AppendUint16(header, uint16(len(body)))
		if _, err := conn.Write(append(header, body...)); err != nil {
			t.Fatal(err)
		}
	}
	send(4, []byte("v=2\nclient=independent-test"))
	send(1, nil)
	magic := "sp.v2.udp-over-tcp.arpa"
	socksAddr := append([]byte{3, byte(len(magic))}, []byte(magic)...)
	socksAddr = append(socksAddr, 0, 0)
	send(2, socksAddr)
	// UoT v2: connect flag + SOCKS address, followed by length-prefixed datagrams.
	addr := protocolTUICDestination(t, destination)
	send(2, append([]byte{1}, addr...))
	payload := []byte("anytls-udp-independent-client")
	send(2, append(binary.BigEndian.AppendUint16(nil, uint16(len(payload))), payload...))
	for {
		var header [7]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, int(binary.BigEndian.Uint16(header[5:])))
		if _, err := io.ReadFull(conn, body); err != nil {
			t.Fatal(err)
		}
		if header[0] == 3 {
			t.Fatal("AnyTLS rejected UDP stream")
		}
		if header[0] == 2 && bytes.Equal(body, append(binary.BigEndian.AppendUint16(nil, uint16(len(payload))), payload...)) {
			return
		}
	}
}

// The pinned Xray TUIC outbound is a placeholder. Use an independent TUIC v5
// wire client over quic-go to test the v2node server, including TLS verification.
func protocolTUICRoundTrip(t *testing.T, info *panel.NodeInfo, tcp, udp string) {
	t.Helper()
	certPEM, err := os.ReadFile(info.Common.CertInfo.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	root := x509.NewCertPool()
	if !root.AppendCertsFromPEM(certPEM) {
		t.Fatal("invalid test certificate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, fmt.Sprintf("127.0.0.1:%d", info.Common.ServerPort), &tls.Config{
		RootCAs: root, ServerName: "localhost", NextProtos: []string{"h3"}, MinVersion: tls.VersionTLS13,
	}, &quic.Config{EnableDatagrams: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "test complete")
	id, _ := uuid.ParseString(protocolTestUUID)
	state := conn.ConnectionState().TLS
	token, err := state.ExportKeyingMaterial(string(id[:]), []byte(protocolTestUUID), 32)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	header := append([]byte{5, 0}, id[:]...)
	header = append(header, token...)
	if _, err := auth.Write(header); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}
	t.Run("tcp", func(t *testing.T) {
		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		stream.SetDeadline(time.Now().Add(8 * time.Second))
		if _, err := stream.Write(append([]byte{5, 1}, protocolTUICDestination(t, tcp)...)); err != nil {
			t.Fatal(err)
		}
		payload := []byte("tuic-tcp-echo")
		if _, err := stream.Write(payload); err != nil {
			t.Fatal(err)
		}
		response := make([]byte, len(payload))
		if _, err := io.ReadFull(stream, response); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, response) {
			t.Fatal("TUIC TCP mismatch")
		}
	})
	t.Run("udp", func(t *testing.T) {
		payload := []byte("tuic-udp-echo")
		packet := []byte{5, 2, 0, 1, 0, 1, 1, 0, 0, byte(len(payload))}
		packet = append(packet, protocolTUICDestination(t, udp)...)
		packet = append(packet, payload...)
		if err := conn.SendDatagram(packet); err != nil {
			t.Fatal(err)
		}
		response, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response) < 17 || !bytes.Equal(response[17:], payload) {
			t.Fatalf("TUIC UDP mismatch: %x", response)
		}
	})
}

func protocolTUICDestination(t *testing.T, address string) []byte {
	t.Helper()
	dest, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	encoded := append([]byte{1}, dest.IP.To4()...)
	return binary.BigEndian.AppendUint16(encoded, uint16(dest.Port))
}

func protocolFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func protocolEchoServers(t *testing.T) (string, string) {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
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
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udp.Close() })
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, a, err := udp.ReadFrom(buffer)
			if err != nil {
				return
			}
			udp.WriteTo(buffer[:n], a)
		}
	}()
	return tcp.Addr().String(), udp.LocalAddr().String()
}

func protocolTestCertificate(t *testing.T) *panel.CertInfo {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cert := &panel.CertInfo{CertMode: "file", CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem")}
	if err := os.WriteFile(cert.CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cert.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return cert
}
