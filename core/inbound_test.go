package core

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/app/proxyman"
	coreConf "github.com/xtls/xray-core/infra/conf"
)

func TestUnmarshalNetworkSettingsLegacyArray(t *testing.T) {
	var got coreConf.TCPConfig
	if err := unmarshalNetworkSettings(json.RawMessage(`[{"acceptProxyProtocol":true}]`), &got); err != nil {
		t.Fatalf("decode legacy network settings: %v", err)
	}
}

func TestUnmarshalNetworkSettingsEmptyArray(t *testing.T) {
	var got coreConf.TCPConfig
	if err := unmarshalNetworkSettings(json.RawMessage(`[]`), &got); err != nil {
		t.Fatalf("decode empty network settings: %v", err)
	}
}

func buildSniffingSettings(t *testing.T, routes []panel.Route) *proxyman.SniffingConfig {
	t.Helper()
	inbound, err := buildInbound(&panel.NodeInfo{
		Type:     "shadowsocks",
		Security: panel.None,
		Common: &panel.CommonNode{
			ListenIP:   "0.0.0.0",
			ServerPort: 443,
			Cipher:     "aes-128-gcm",
			Routes:     routes,
		},
	}, "test-inbound")
	if err != nil {
		t.Fatalf("build inbound: %v", err)
	}

	instance, err := inbound.ReceiverSettings.GetInstance()
	if err != nil {
		t.Fatalf("decode receiver settings: %v", err)
	}
	receiver, ok := instance.(*proxyman.ReceiverConfig)
	if !ok {
		t.Fatalf("unexpected receiver settings type %T", instance)
	}
	return receiver.GetSniffingSettings()
}

func TestHysteriaObfsPasswordIsJSONEncoded(t *testing.T) {
	inbound := &coreConf.InboundDetourConfig{}
	password := `"}],"unexpected":true,"password":"still-data`
	err := buildHysteria2(&panel.NodeInfo{Common: &panel.CommonNode{
		Obfs:         "salamander",
		ObfsPassword: password,
	}}, inbound)
	if err != nil {
		t.Fatalf("build hysteria2: %v", err)
	}
	if inbound.StreamSetting == nil || inbound.StreamSetting.FinalMask == nil ||
		len(inbound.StreamSetting.FinalMask.Udp) != 1 ||
		inbound.StreamSetting.FinalMask.Udp[0].Settings == nil {
		t.Fatal("missing hysteria2 obfs settings")
	}

	var settings map[string]string
	if err := json.Unmarshal(*inbound.StreamSetting.FinalMask.Udp[0].Settings, &settings); err != nil {
		t.Fatalf("obfs settings are invalid JSON: %v", err)
	}
	if settings["password"] != password || len(settings) != 1 {
		t.Fatalf("obfs password escaped its JSON field: %#v", settings)
	}
}

func generateTestCert(t *testing.T) (string, string) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.cer")
	keyPath := filepath.Join(dir, "key.key")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		Version:      3,
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(1, 0, 0),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestBuildHysteria2EnforcesTLSAndH3(t *testing.T) {
	certFile, keyFile := generateTestCert(t)
	// Simulate V2Board node where Tls: 0 and CertInfo has no CertMode
	for _, proto := range []string{"hysteria2", "hysteria", "hy2"} {
		nodeInfo := &panel.NodeInfo{
			Id:       1,
			Type:     proto,
			Security: panel.None, // Panel sent tls = 0
			Common: &panel.CommonNode{
				Protocol:   proto,
				ListenIP:   "0.0.0.0",
				ServerPort: 443,
				CertInfo: &panel.CertInfo{
					CertFile: certFile,
					KeyFile:  keyFile,
				},
			},
		}
		inbound, err := buildInbound(nodeInfo, "hys-tag")
		if err != nil {
			t.Fatalf("buildInbound failed for proto %s: %v", proto, err)
		}
		if inbound == nil {
			t.Fatalf("inbound is nil for proto %s", proto)
		}
		receiverInstance, err := inbound.ReceiverSettings.GetInstance()
		if err != nil {
			t.Fatalf("failed to decode receiver settings for proto %s: %v", proto, err)
		}
		receiver, ok := receiverInstance.(*proxyman.ReceiverConfig)
		if !ok || receiver.StreamSettings == nil {
			t.Fatalf("missing stream settings for proto %s", proto)
		}
		if receiver.StreamSettings.SecurityType == "" {
			t.Fatalf("expected TLS security type for proto %s, got empty", proto)
		}
	}
}

func TestBuildInboundDisablesSniffingWithoutDomainRoutes(t *testing.T) {
	sniffing := buildSniffingSettings(t, nil)
	if sniffing == nil {
		t.Fatal("missing sniffing settings")
	}
	if sniffing.GetEnabled() {
		t.Fatal("ordinary nodes must not rewrite Meta or other CDN destinations")
	}
}

func TestBuildInboundUsesRouteOnlySniffingForDomainRoutes(t *testing.T) {
	sniffing := buildSniffingSettings(t, []panel.Route{{
		Action: "route",
		Match:  []string{"domain:tiktok.com"},
	}})
	if sniffing == nil || !sniffing.GetEnabled() {
		t.Fatal("domain routes require content sniffing")
	}
	if !sniffing.GetRouteOnly() {
		t.Fatal("sniffing must not replace the client destination through the VPS resolver")
	}

	overrides := make(map[string]bool, len(sniffing.GetDestinationOverride()))
	for _, protocol := range sniffing.GetDestinationOverride() {
		overrides[protocol] = true
	}
	for _, protocol := range []string{"http", "tls", "quic"} {
		if !overrides[protocol] {
			t.Fatalf("missing %s destination override: %v", protocol, sniffing.GetDestinationOverride())
		}
	}
}

func TestBuildInboundRejectsUnknownSecurityMode(t *testing.T) {
	if _, err := buildInbound(&panel.NodeInfo{
		Type:     "vmess",
		Security: 99,
		Common:   &panel.CommonNode{ListenIP: "127.0.0.1", ServerPort: 443},
	}, "node"); err == nil {
		t.Fatal("unknown transport security mode fell back to plaintext")
	}
}

func TestBuildInboundAcceptsLegacyArrayNetworkSettings(t *testing.T) {
	if _, err := buildInbound(&panel.NodeInfo{
		Type:     "vmess",
		Security: panel.None,
		Common: &panel.CommonNode{
			ListenIP:        "0.0.0.0",
			ServerPort:      443,
			Network:         "tcp",
			NetworkSettings: json.RawMessage(`[{"acceptProxyProtocol":false}]`),
		},
	}, "legacy-array"); err != nil {
		t.Fatalf("legacy array network settings must build an inbound: %v", err)
	}
}

func TestForwardedClientIPAllowsConfiguredPublicOrLoopbackListener(t *testing.T) {
	proxySettings := json.RawMessage(`{"acceptProxyProtocol":true}`)
	for name, mutate := range map[string]func(*panel.CommonNode){
		"proxy protocol": func(common *panel.CommonNode) {
			common.NetworkSettings = proxySettings
		},
		"forwarded header": func(common *panel.CommonNode) {
			common.TrustedXForwardedFor = []string{"CF-Connecting-IP"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			common := &panel.CommonNode{
				ListenIP:        "0.0.0.0",
				ServerPort:      443,
				Network:         "tcp",
				NetworkSettings: json.RawMessage(`{}`),
			}
			mutate(common)
			if _, err := buildInbound(&panel.NodeInfo{
				Type: "vmess", Security: panel.None, Common: common,
			}, "public-listener"); err != nil {
				t.Fatalf("configured public listener was rejected: %v", err)
			}

			common.ListenIP = "127.0.0.1"
			if _, err := buildInbound(&panel.NodeInfo{
				Type: "vmess", Security: panel.None, Common: common,
			}, "loopback-listener"); err != nil {
				t.Fatalf("trusted local proxy setup was rejected: %v", err)
			}
		})
	}
}

func TestAllVPNProtocolsInboundAndUsers(t *testing.T) {
	certFile, keyFile := generateTestCert(t)
	dummyCert := &panel.CertInfo{
		CertMode: "self",
		CertFile: certFile,
		KeyFile:  keyFile,
	}

	testCases := []struct {
		name      string
		nodeInfo  *panel.NodeInfo
		users     []panel.UserInfo
		checkUser bool
	}{
		{
			name: "VMess TCP Plaintext",
			nodeInfo: &panel.NodeInfo{
				Id: 1, Type: "vmess", Security: panel.None,
				Common: &panel.CommonNode{
					ListenIP: "0.0.0.0", ServerPort: 10001, Network: "tcp",
				},
			},
			users:     []panel.UserInfo{{Id: 1, Uuid: "d290f1ee-6c54-4b01-90e6-d701748f0851"}},
			checkUser: true,
		},
		{
			name: "VMess WS TLS",
			nodeInfo: &panel.NodeInfo{
				Id: 2, Type: "vmess", Security: panel.Tls,
				Common: &panel.CommonNode{
					ListenIP: "0.0.0.0", ServerPort: 10002, Network: "ws",
					NetworkSettings: json.RawMessage(`{"path":"/vmessws"}`),
					CertInfo:        dummyCert,
				},
			},
			users:     []panel.UserInfo{{Id: 2, Uuid: "d290f1ee-6c54-4b01-90e6-d701748f0852"}},
			checkUser: true,
		},
		{
			name: "VLESS Reality TCP",
			nodeInfo: &panel.NodeInfo{
				Id: 3, Type: "vless", Security: panel.Reality,
				Common: &panel.CommonNode{
					ListenIP: "0.0.0.0", ServerPort: 10003, Network: "tcp",
					TlsSettings: panel.TlsSettings{
						ServerName: "www.microsoft.com",
						Dest:       "www.microsoft.com",
						ServerPort: "443",
						PrivateKey: "1DPuqNuqAeziTDmHOAg5EeU3bAisao0j26np6HOLTYU",
						ShortId:    "2e47c7ee",
					},
				},
			},
			users:     []panel.UserInfo{{Id: 3, Uuid: "d290f1ee-6c54-4b01-90e6-d701748f0853"}},
			checkUser: true,
		},
		{
			name: "VLESS WS TLS",
			nodeInfo: &panel.NodeInfo{
				Id: 4, Type: "vless", Security: panel.Tls,
				Common: &panel.CommonNode{
					ListenIP: "0.0.0.0", ServerPort: 10004, Network: "ws",
					NetworkSettings: json.RawMessage(`{"path":"/vlessws"}`),
					CertInfo:        dummyCert,
				},
			},
			users:     []panel.UserInfo{{Id: 4, Uuid: "d290f1ee-6c54-4b01-90e6-d701748f0854"}},
			checkUser: true,
		},
		{
			name: "Trojan TCP TLS",
			nodeInfo: &panel.NodeInfo{
				Id: 5, Type: "trojan", Security: panel.Tls,
				Common: &panel.CommonNode{
					ListenIP: "0.0.0.0", ServerPort: 10005, Network: "tcp",
					CertInfo: dummyCert,
				},
			},
			users:     []panel.UserInfo{{Id: 5, Uuid: "password12345"}},
			checkUser: true,
		},
		{
			name: "Shadowsocks AEAD (aes-128-gcm)",
			nodeInfo: &panel.NodeInfo{
				Id: 6, Type: "shadowsocks", Security: panel.None,
				Common: &panel.CommonNode{
					ListenIP: "0.0.0.0", ServerPort: 10006, Cipher: "aes-128-gcm",
				},
			},
			users:     []panel.UserInfo{{Id: 6, Uuid: "secretpassword"}},
			checkUser: true,
		},
		{
			name: "Shadowsocks 2022 (2022-blake3-aes-128-gcm)",
			nodeInfo: &panel.NodeInfo{
				Id: 7, Type: "shadowsocks", Security: panel.None,
				Common: &panel.CommonNode{
					ListenIP: "0.0.0.0", ServerPort: 10007,
					Cipher:    "2022-blake3-aes-128-gcm",
					ServerKey: "0123456789abcdef",
				},
			},
			users:     []panel.UserInfo{{Id: 7, Uuid: "0123456789abcdef"}},
			checkUser: true,
		},
		{
			name: "Hysteria 2 (QUIC + Auto TLS + Brutal)",
			nodeInfo: &panel.NodeInfo{
				Id: 8, Type: "hysteria2", Security: panel.Tls,
				Common: &panel.CommonNode{
					ListenIP: "0.0.0.0", ServerPort: 10008,
					CertInfo: dummyCert,
					UpMbps:   100, DownMbps: 100,
				},
			},
			users:     []panel.UserInfo{{Id: 8, Uuid: "auth-token-1234"}},
			checkUser: true,
		},
		{
			name: "Tuic (QUIC + Auto TLS + BBR)",
			nodeInfo: &panel.NodeInfo{
				Id: 9, Type: "tuic", Security: panel.Tls,
				Common: &panel.CommonNode{
					ListenIP: "0.0.0.0", ServerPort: 10009,
					CertInfo: dummyCert,
					CongestionControl: "bbr",
				},
			},
			users:     []panel.UserInfo{{Id: 9, Uuid: "d290f1ee-6c54-4b01-90e6-d701748f0859"}},
			checkUser: true,
		},
		{
			name: "AnyTLS TCP TLS",
			nodeInfo: &panel.NodeInfo{
				Id: 10, Type: "anytls", Security: panel.Tls,
				Common: &panel.CommonNode{
					ListenIP: "0.0.0.0", ServerPort: 10010, Network: "tcp",
					CertInfo: dummyCert,
				},
			},
			users:     []panel.UserInfo{{Id: 10, Uuid: "anytls-password"}},
			checkUser: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			inbound, err := buildInbound(tc.nodeInfo, tc.name)
			if err != nil {
				t.Fatalf("[%s] buildInbound failed: %v", tc.name, err)
			}
			if inbound == nil {
				t.Fatalf("[%s] inbound is nil", tc.name)
			}

			if tc.checkUser {
				p := &AddUsersParams{
					Tag:      tc.name,
					NodeInfo: tc.nodeInfo,
					Users:    tc.users,
				}
				switch tc.nodeInfo.Type {
				case "vmess":
					users := buildVmessUsers(p.Tag, p.Users)
					if len(users) != len(tc.users) {
						t.Fatalf("expected %d vmess users, got %d", len(tc.users), len(users))
					}
				case "vless":
					users := buildVlessUsers(p.Tag, p.Users, p.Common.Flow)
					if len(users) != len(tc.users) {
						t.Fatalf("expected %d vless users, got %d", len(tc.users), len(users))
					}
				case "trojan":
					users := buildTrojanUsers(p.Tag, p.Users)
					if len(users) != len(tc.users) {
						t.Fatalf("expected %d trojan users, got %d", len(tc.users), len(users))
					}
				case "shadowsocks":
					users, err := buildSSUsers(p.Tag, p.Users, p.Common.Cipher, p.Common.ServerKey)
					if err != nil {
						t.Fatalf("buildSSUsers failed: %v", err)
					}
					if len(users) != len(tc.users) {
						t.Fatalf("expected %d ss users, got %d", len(tc.users), len(users))
					}
				case "hysteria2", "hysteria", "hy2":
					users := buildHysteria2Users(p.Tag, p.Users)
					if len(users) != len(tc.users) {
						t.Fatalf("expected %d hysteria2 users, got %d", len(tc.users), len(users))
					}
				case "tuic":
					users := buildTuicUsers(p.Tag, p.Users)
					if len(users) != len(tc.users) {
						t.Fatalf("expected %d tuic users, got %d", len(tc.users), len(users))
					}
				case "anytls":
					users := buildAnyTLSUsers(p.Tag, p.Users)
					if len(users) != len(tc.users) {
						t.Fatalf("expected %d anytls users, got %d", len(tc.users), len(users))
					}
				}
			}
		})
	}
}

func TestLiveNode81ConfigAndWireGuardBalancer(t *testing.T) {
	node81JSON := []byte(`{"listen_ip":"0.0.0.0","server_port":830,"network":"tcp","network_settings":null,"protocol":"vless","tls":2,"tls_settings":{"server_name":"www.lonza.com","cert_mode":"self","provider":null,"dns_env":null,"reject_unknown_sni":"0","allow_insecure":"0","public_key":"djJRsFrR16Wcoc1UH8dgBbACt_W8B_Nk7nBdwPNSfBM","private_key":"1DPuqNuqAeziTDmHOAg5EeU3bAisao0j26np6HOLTYU","short_id":"2e47c7ee","server_port":"443"},"encryption":null,"encryption_settings":null,"flow":null,"cipher":null,"congestion_control":null,"zero_rtt_handshake":false,"up_mbps":0,"down_mbps":0,"obfs":null,"obfs_password":null,"padding_scheme":null,"ignore_client_bandwidth":true,"base_config":{"push_interval":60,"pull_interval":60,"node_report_min_traffic":0,"device_online_min_traffic":0},"routes":[{"id":23,"match":[],"action":"default_out","action_value":"[\n{\n  \"tag\": \"vn1\",\n  \"protocol\": \"wireguard\",\n  \"settings\": {\n    \"secretKey\": \"COYrxmQRV27b\/5XUMrhxa70XhkT5JFakYqLARDNkSW4=\",\n    \"address\": [\n      \"10.2.0.2\/32\"\n    ],\n    \"peers\": [\n      {\n        \"publicKey\": \"NfKOMtk2fuDycbQXv36yk5mfdgDA8\/8SN6amCdFrKxQ=\",\n        \"endpoint\": \"188.214.152.226:51820\",\n        \"allowedIPs\": [\n          \"0.0.0.0\/0\"\n        ],\n        \"keepAlive\": 25\n      }\n    ],\n    \"mtu\": 1280\n  }\n},\n{\n  \"tag\": \"vn2\",\n  \"protocol\": \"wireguard\",\n  \"settings\": {\n    \"secretKey\": \"YBm1l32BOxu9FWWlMhvf9F0NiXlivgEwGJv5MWgeVUM=\",\n    \"address\": [\n      \"10.2.0.2\/32\"\n    ],\n    \"peers\": [\n      {\n        \"publicKey\": \"NfKOMtk2fuDycbQXv36yk5mfdgDA8\/8SN6amCdFrKxQ=\",\n        \"endpoint\": \"188.214.152.226:51820\",\n        \"allowedIPs\": [\n          \"0.0.0.0\/0\"\n        ],\n        \"keepAlive\": 25\n      }\n    ],\n    \"mtu\": 1280\n  }\n},\n{\n  \"tag\": \"vn3\",\n  \"protocol\": \"wireguard\",\n  \"settings\": {\n    \"secretKey\": \"SITRSWxBHekZMwdbw+cc46cwpr2btFJ8WUhq2Z0X10A=\",\n    \"address\": [\n      \"10.2.0.2\/32\"\n    ],\n    \"peers\": [\n      {\n        \"publicKey\": \"NfKOMtk2fuDycbQXv36yk5mfdgDA8\/8SN6amCdFrKxQ=\",\n        \"endpoint\": \"188.214.152.226:51820\",\n        \"allowedIPs\": [\n          \"0.0.0.0\/0\"\n        ],\n        \"keepAlive\": 25\n      }\n    ],\n    \"mtu\": 1280\n  }\n}\n]"}]}`)

	var cm panel.CommonNode
	if err := json.Unmarshal(node81JSON, &cm); err != nil {
		t.Fatalf("unmarshal node 81 json: %v", err)
	}

	nodeInfo := &panel.NodeInfo{
		Id:       81,
		Type:     "vless",
		Security: panel.Reality,
		Tag:      "node-81",
		Common:   &cm,
	}

	inbound, err := buildInbound(nodeInfo, "node-81")
	if err != nil {
		t.Fatalf("buildInbound for node 81 failed: %v", err)
	}
	if inbound == nil {
		t.Fatal("inbound is nil for node 81")
	}

	dnsConfig, outbounds, routerConfig, obsConfig, defaultTags, err := GetCustomConfig([]*panel.NodeInfo{nodeInfo})
	if err != nil {
		t.Fatalf("GetCustomConfig for node 81 failed: %v", err)
	}
	if dnsConfig == nil || routerConfig == nil {
		t.Fatal("dnsConfig or routerConfig is nil")
	}
	if len(defaultTags) != 3 {
		t.Fatalf("expected 3 defaultTags for WireGuard (vn1, vn2, vn3), got %v", defaultTags)
	}
	if obsConfig == nil {
		t.Fatal("expected observatory config to be generated for multiple WireGuard routes")
	}
	// Check outbounds has vn1, vn2, vn3
	foundWG := 0
	for _, o := range outbounds {
		if o.Tag == "vn1" || o.Tag == "vn2" || o.Tag == "vn3" {
			foundWG++
		}
	}
	if foundWG != 3 {
		t.Fatalf("expected 3 wireguard outbounds, found %d", foundWG)
	}
}

