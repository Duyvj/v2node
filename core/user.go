package core

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/counter"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/core/app/dispatcher"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/proxy/anytls"
	hyaccount "github.com/xtls/xray-core/proxy/hysteria/account"
	"github.com/xtls/xray-core/proxy/shadowsocks"
	"github.com/xtls/xray-core/proxy/shadowsocks_2022"
	"github.com/xtls/xray-core/proxy/trojan"
	"github.com/xtls/xray-core/proxy/tuic"
	"github.com/xtls/xray-core/proxy/vless"
)

func (v *V2Core) GetUserManager(tag string) (proxy.UserManager, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handler, err := v.ihm.GetHandler(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("no such inbound tag: %s", err)
	}
	inboundInstance, ok := handler.(proxy.GetInbound)
	if !ok {
		return nil, fmt.Errorf("handler %s is not implement proxy.GetInbound", tag)
	}
	userManager, ok := inboundInstance.GetInbound().(proxy.UserManager)
	if !ok {
		return nil, fmt.Errorf("handler %s is not implement proxy.UserManager", tag)
	}
	return userManager, nil
}

func (vc *V2Core) DelUsers(users []panel.UserInfo, tag string, _ *panel.NodeInfo) error {
	userManager, err := vc.GetUserManager(tag)
	if err != nil {
		return fmt.Errorf("get user manager error: %s", err)
	}
	var user string
	vc.users.mapLock.Lock()
	defer vc.users.mapLock.Unlock()
	for i := range users {
		user = format.UserTag(tag, users[i].Uuid)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = userManager.RemoveUser(ctx, user)
		cancel()
		if err != nil {
			return err
		}
		delete(vc.users.uidMap, user)
		if v, ok := vc.dispatcher.Counter.Load(tag); ok {
			tc := v.(*counter.TrafficCounter)
			tc.Delete(user)
		}
		if v, ok := vc.dispatcher.LinkManagers.Load(user); ok {
			lm := v.(*dispatcher.LinkManager)
			lm.CloseAll()
			vc.dispatcher.LinkManagers.Delete(user)
		}
	}
	return nil
}

// SnapshotUserTraffic captures exact counter identities, not only numeric UIDs.
// Acknowledgement subtracts once and preserves concurrent additions.
func (vc *V2Core) SnapshotUserTraffic(tag string, mintraffic int) ([]panel.UserTraffic, func()) {
	type sample struct {
		counter  *counter.TrafficStorage
		up, down int64
	}
	var samples []sample
	traffic := make([]panel.UserTraffic, 0, 64)
	vc.users.mapLock.RLock()
	if v, ok := vc.dispatcher.Counter.Load(tag); ok {
		c := v.(*counter.TrafficCounter)
		c.Counters.Range(func(key, value any) bool {
			id := vc.users.uidMap[key.(string)]
			if id == 0 {
				return true
			}
			s := value.(*counter.TrafficStorage)
			up, down := s.UpCounter.Load(), s.DownCounter.Load()
			if up+down > int64(mintraffic)*1000 {
				samples = append(samples, sample{s, up, down})
				traffic = append(traffic, panel.UserTraffic{UID: id, Upload: up, Download: down})
			}
			return true
		})
	}
	vc.users.mapLock.RUnlock()
	var once sync.Once
	commit := func() {
		once.Do(func() {
			for _, s := range samples {
				s.counter.UpCounter.Add(-s.up)
				s.counter.DownCounter.Add(-s.down)
			}
		})
	}
	return traffic, commit
}
func (vc *V2Core) GetUserTrafficSlice(tag string, mintraffic int) ([]panel.UserTraffic, error) {
	traffic, _ := vc.SnapshotUserTraffic(tag, mintraffic)
	return traffic, nil
}

func (v *V2Core) AddUsers(p *AddUsersParams) (added int, err error) {
	if p == nil || p.NodeInfo == nil || p.Common == nil {
		return 0, fmt.Errorf("missing user node configuration")
	}
	man, err := v.GetUserManager(p.Tag)
	if err != nil {
		return 0, fmt.Errorf("get user manager: %w", err)
	}
	v.users.mapLock.Lock()
	defer v.users.mapLock.Unlock()
	// Convert one account at a time: do not retain a second complete user list.
	// Roll back a partially rejected batch so a panel retry can apply it again.
	defer func() {
		if err == nil {
			return
		}
		for i := 0; i < added; i++ {
			key := format.UserTag(p.Tag, p.Users[i].Uuid)
			if removeErr := man.RemoveUser(context.Background(), key); removeErr != nil {
				err = fmt.Errorf("%w; rollback user: %v", err, removeErr)
			}
			delete(v.users.uidMap, key)
		}
		added = 0
	}()
	for i := range p.Users {
		key := format.UserTag(p.Tag, p.Users[i].Uuid)
		if _, exists := v.users.uidMap[key]; exists {
			return added, fmt.Errorf("user credential already registered")
		}
		var u *protocol.User
		u, err = buildUser(p, &p.Users[i])
		if err != nil {
			return added, err
		}
		var memoryUser *protocol.MemoryUser
		memoryUser, err = u.ToMemoryUser()
		if err != nil {
			return added, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = man.AddUser(ctx, memoryUser)
		cancel()
		if err != nil {
			return added, err
		}
		v.users.uidMap[key] = p.Users[i].Id
		added++
	}
	// The pinned AnyTLS core applies a batch asynchronously after a debounce.
	// Report success only once its published authentication snapshot is ready.
	if p.Type == "anytls" && added > 0 {
		key := format.UserTag(p.Tag, p.Users[added-1].Uuid)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for man.GetUser(ctx, key) == nil {
			select {
			case <-ctx.Done():
				return added, fmt.Errorf("activate anytls users: %w", ctx.Err())
			case <-ticker.C:
			}
		}
	}
	return added, nil
}

func buildUser(p *AddUsersParams, u *panel.UserInfo) (*protocol.User, error) {
	switch p.Type {
	case "vmess":
		return buildVmessUser(p.Tag, u), nil
	case "vless":
		return buildVlessUser(p.Tag, u, p.Common.Flow), nil
	case "trojan":
		return buildTrojanUser(p.Tag, u), nil
	case "shadowsocks":
		if p.Common.ServerKey != "" {
			size, err := shadowsocksKeyLength(p.Common.Cipher)
			if err != nil {
				return nil, err
			}
			if len(u.Uuid) < size {
				return nil, fmt.Errorf("shadowsocks 2022 credential must contain at least %d bytes", size)
			}
		}
		return buildSSUser(p.Tag, u, p.Common.Cipher, p.Common.ServerKey), nil
	case "hysteria2":
		return buildHysteria2User(p.Tag, u), nil
	case "tuic":
		return buildTuicUser(p.Tag, u), nil
	case "anytls":
		return buildAnyTLSUser(p.Tag, u), nil
	default:
		return nil, fmt.Errorf("unsupported node type: %s", p.Type)
	}
}

func shadowsocksKeyLength(cipher string) (int, error) {
	switch cipher {
	case "2022-blake3-aes-128-gcm":
		return 16, nil
	case "2022-blake3-aes-256-gcm":
		return 32, nil
	default:
		return 0, fmt.Errorf("unsupported shadowsocks 2022 multi-user cipher: %s", cipher)
	}
}

func buildVmessUser(tag string, userInfo *panel.UserInfo) (user *protocol.User) {
	vmessAccount := &conf.VMessAccount{
		ID:       userInfo.Uuid,
		Security: "auto",
	}
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(vmessAccount.Build()),
	}
}

func buildVlessUser(tag string, userInfo *panel.UserInfo, flow string) (user *protocol.User) {
	vlessAccount := &vless.Account{
		Id: userInfo.Uuid,
	}
	vlessAccount.Flow = flow
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(vlessAccount),
	}
}

func buildTrojanUser(tag string, userInfo *panel.UserInfo) (user *protocol.User) {
	trojanAccount := &trojan.Account{
		Password: userInfo.Uuid,
	}
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(trojanAccount),
	}
}

func buildSSUser(tag string, userInfo *panel.UserInfo, cypher string, serverKey string) (user *protocol.User) {
	if serverKey == "" {
		ssAccount := &shadowsocks.Account{
			Password:   userInfo.Uuid,
			CipherType: getCipherFromString(cypher),
		}
		return &protocol.User{
			Level:   0,
			Email:   format.UserTag(tag, userInfo.Uuid),
			Account: serial.ToTypedMessage(ssAccount),
		}
	} else {
		keyLength, _ := shadowsocksKeyLength(cypher)
		ssAccount := &shadowsocks_2022.Account{
			Key: base64.StdEncoding.EncodeToString([]byte(userInfo.Uuid[:keyLength])),
		}
		return &protocol.User{
			Level:   0,
			Email:   format.UserTag(tag, userInfo.Uuid),
			Account: serial.ToTypedMessage(ssAccount),
		}
	}
}

func getCipherFromString(c string) shadowsocks.CipherType {
	switch strings.ToLower(c) {
	case "aes-128-gcm", "aead_aes_128_gcm":
		return shadowsocks.CipherType_AES_128_GCM
	case "aes-256-gcm", "aead_aes_256_gcm":
		return shadowsocks.CipherType_AES_256_GCM
	case "chacha20-poly1305", "aead_chacha20_poly1305", "chacha20-ietf-poly1305":
		return shadowsocks.CipherType_CHACHA20_POLY1305
	case "xchacha20-poly1305", "aead_xchacha20_poly1305", "xchacha20-ietf-poly1305":
		return shadowsocks.CipherType_XCHACHA20_POLY1305
	default:
		return shadowsocks.CipherType_UNKNOWN
	}
}

func buildHysteria2User(tag string, userInfo *panel.UserInfo) (user *protocol.User) {
	hysteria2Account := &hyaccount.Account{
		Auth: userInfo.Uuid,
	}
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(hysteria2Account),
	}
}

func buildTuicUser(tag string, userInfo *panel.UserInfo) (user *protocol.User) {
	tuicAccount := &tuic.Account{
		Uuid:     userInfo.Uuid,
		Password: userInfo.Uuid,
	}
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(tuicAccount),
	}
}

func buildAnyTLSUser(tag string, userInfo *panel.UserInfo) (user *protocol.User) {
	anyTLSAccount := &anytls.Account{
		Password: userInfo.Uuid,
	}
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(anyTLSAccount),
	}
}
