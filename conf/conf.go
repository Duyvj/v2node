package conf

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/viper"
)

const DefaultNodeRetryCount = 1
const DefaultNodeTimeout = 15

type Conf struct {
	LogConfig        LogConfig        `mapstructure:"Log"`
	NodeConfigs      []NodeConfig     `mapstructure:"Nodes"`
	PprofPort        int              `mapstructure:"PprofPort"`
	ConnectionConfig ConnectionConfig `mapstructure:"ConnectionConfig"`
	ResourceConfig   ResourceConfig   `mapstructure:"Resource"`
	watchCancel      context.CancelFunc
	watchDone        chan struct{}
}

// Defaults preserve upstream behavior. Memory and sniffing changes are opt-in.
type ConnectionConfig struct {
	Handshake            uint32 `mapstructure:"Handshake"`
	ConnIdle             uint32 `mapstructure:"ConnIdle"`
	UplinkOnly           uint32 `mapstructure:"UplinkOnly"`
	DownlinkOnly         uint32 `mapstructure:"DownlinkOnly"`
	BufferSize           int32  `mapstructure:"BufferSize"`
	MetadataOnlySniffing bool   `mapstructure:"MetadataOnlySniffing"`
}
type ResourceConfig struct {
	GOGC          int   `mapstructure:"GOGC"`
	MemoryLimitMB int64 `mapstructure:"MemoryLimitMB"`
}

type LogConfig struct {
	Level  string `mapstructure:"Level"`
	Output string `mapstructure:"Output"`
	Access string `mapstructure:"Access"`
}

type NodeConfig struct {
	APIHost    string `mapstructure:"ApiHost"`
	NodeID     int    `mapstructure:"NodeID"`
	Key        string `mapstructure:"ApiKey"`
	Timeout    int    `mapstructure:"Timeout"`
	RetryCount *int   `mapstructure:"RetryCount"`
}

func New() *Conf {
	return &Conf{
		ConnectionConfig: ConnectionConfig{Handshake: 4, ConnIdle: 120, UplinkOnly: 2, DownlinkOnly: 4, BufferSize: 128},
		LogConfig: LogConfig{
			Level:  "info",
			Output: "",
			Access: "none",
		},
	}
}

func (p *Conf) LoadFromPath(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open config file error: %s", err)
	}
	defer f.Close()
	v := viper.New()
	v.SetConfigFile(filePath)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config file error: %s", err)
	}
	if err := v.Unmarshal(p); err != nil {
		return fmt.Errorf("unmarshal config error: %s", err)
	}
	for i := range p.NodeConfigs {
		if p.NodeConfigs[i].RetryCount == nil {
			p.NodeConfigs[i].RetryCount = intPtr(DefaultNodeRetryCount)
		}
	}
	if len(p.NodeConfigs) == 0 {
		return fmt.Errorf("at least one node is required")
	}
	if p.ConnectionConfig.BufferSize < 0 || p.ConnectionConfig.BufferSize > 4096 {
		return fmt.Errorf("BufferSize must be 0..4096 KiB")
	}
	if p.ConnectionConfig.Handshake == 0 || p.ConnectionConfig.ConnIdle == 0 {
		return fmt.Errorf("Handshake and ConnIdle must be positive")
	}
	if p.ResourceConfig.GOGC < 0 || p.ResourceConfig.GOGC > 1000 {
		return fmt.Errorf("GOGC must be 0..1000")
	}
	if p.ResourceConfig.MemoryLimitMB < 0 || p.ResourceConfig.MemoryLimitMB > (1<<30) {
		return fmt.Errorf("MemoryLimitMB is out of range")
	}
	return nil
}

func intPtr(v int) *int {
	return &v
}
