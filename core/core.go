package core

import (
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core/app/dispatcher"
	_ "github.com/wyx2685/v2node/core/distro/all"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/routing"
	coreConf "github.com/xtls/xray-core/infra/conf"
	"google.golang.org/protobuf/proto"
)

type AddUsersParams struct {
	Tag   string
	Users []panel.UserInfo
	*panel.NodeInfo
}

type V2Core struct {
	Config     *conf.Conf
	ReloadCh   chan struct{}
	access     sync.Mutex
	Server     *core.Instance
	users      *UserMap
	ihm        inbound.Manager
	ohm        outbound.Manager
	dispatcher *dispatcher.DefaultDispatcher
}

type UserMap struct {
	uidMap  map[string]int
	mapLock sync.RWMutex
}

func New(config *conf.Conf) *V2Core {
	core := &V2Core{
		Config: config,
		users: &UserMap{
			uidMap: make(map[string]int),
		},
	}
	return core
}

func (v *V2Core) Start(infos []*panel.NodeInfo) error {
	v.access.Lock()
	defer v.access.Unlock()
	var err error
	v.Server, err = getCore(v.Config, infos)
	if err != nil {
		return err
	}
	if err := v.Server.Start(); err != nil {
		return err
	}
	v.ihm = v.Server.GetFeature(inbound.ManagerType()).(inbound.Manager)
	v.ohm = v.Server.GetFeature(outbound.ManagerType()).(outbound.Manager)
	v.dispatcher = v.Server.GetFeature(routing.DispatcherType()).(*dispatcher.DefaultDispatcher)
	v.dispatcher.MetadataOnlySniffing = v.Config.ConnectionConfig.MetadataOnlySniffing
	return nil
}

func (v *V2Core) Close() error {
	v.access.Lock()
	defer v.access.Unlock()
	if v.Server == nil {
		return nil
	}
	v.ihm = nil
	v.ohm = nil
	v.dispatcher = nil
	err := v.Server.Close()
	v.Server = nil
	if err != nil {
		return err
	}
	return nil
}

func getCore(c *conf.Conf, infos []*panel.NodeInfo) (*core.Instance, error) {
	// Log Config
	coreLogConfig := &coreConf.LogConfig{
		LogLevel:  c.LogConfig.Level,
		AccessLog: c.LogConfig.Access,
		ErrorLog:  c.LogConfig.Output,
	}
	// Custom config
	dnsConfig, outBoundConfig, routeConfig, err := GetCustomConfig(infos)
	if err != nil {
		return nil, fmt.Errorf("build custom config: %w", err)
	}
	// Inbound config
	var inBoundConfig []*core.InboundHandlerConfig

	// Policy config
	levelPolicyConfig := &coreConf.Policy{
		StatsUserUplink:   true,
		StatsUserDownlink: true,
		Handshake:         proto.Uint32(c.ConnectionConfig.Handshake),
		ConnectionIdle:    proto.Uint32(c.ConnectionConfig.ConnIdle),
		UplinkOnly:        proto.Uint32(c.ConnectionConfig.UplinkOnly),
		DownlinkOnly:      proto.Uint32(c.ConnectionConfig.DownlinkOnly),
		BufferSize:        proto.Int32(c.ConnectionConfig.BufferSize),
	}
	corePolicyConfig := &coreConf.PolicyConfig{}
	corePolicyConfig.Levels = map[uint32]*coreConf.Policy{0: levelPolicyConfig}
	policyConfig, err := corePolicyConfig.Build()
	if err != nil {
		return nil, err
	}
	// Build Xray conf
	config := &core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(coreLogConfig.Build()),
			serial.ToTypedMessage(&dispatcher.Config{}),
			serial.ToTypedMessage(&stats.Config{}),
			serial.ToTypedMessage(&proxyman.InboundConfig{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
			serial.ToTypedMessage(policyConfig),
			serial.ToTypedMessage(dnsConfig),
			serial.ToTypedMessage(routeConfig),
		},
		Inbound:  inBoundConfig,
		Outbound: outBoundConfig,
	}
	server, err := core.New(config)
	if err != nil {
		return nil, fmt.Errorf("create core: %w", err)
	}
	log.Info("Xray Core Version: ", core.Version())
	return server, nil
}
