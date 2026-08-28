package conf

import (
	"fmt"
	"os"

	"github.com/spf13/viper"
)

const DefaultNodeRetryCount = 1
const DefaultNodeTimeout = 15

type Conf struct {
	LogConfig      LogConfig      `mapstructure:"Log"`
	NodeConfigs    []NodeConfig   `mapstructure:"Nodes"`
	ResourceConfig ResourceConfig `mapstructure:"Resource"`
	PprofPort      int            `mapstructure:"PprofPort"`
}

type ResourceConfig struct {
	Profile                       string `mapstructure:"Profile"`                       // "low", "standard", "high_performance"
	MemLimitMB                    int    `mapstructure:"MemLimitMB"`                    // Max memory target in MB (0 to disable)
	GOGC                          int    `mapstructure:"GOGC"`                          // GC percent (e.g., 50 for low RAM, 80 for standard, 100 for high)
	BufferSize                    int    `mapstructure:"BufferSize"`                    // Buffer size per pipe in KB (e.g., 16, 32, 128)
	ConnectionIdle                int    `mapstructure:"ConnectionIdle"`                // Inactive connection timeout in seconds
	Handshake                     int    `mapstructure:"Handshake"`                     // Handshake timeout in seconds
	UplinkOnly                    int    `mapstructure:"UplinkOnly"`                    // Uplink only timeout in seconds
	DownlinkOnly                  int    `mapstructure:"DownlinkOnly"`                  // Downlink only timeout in seconds
	PeriodicMemoryReleaseInterval int    `mapstructure:"PeriodicMemoryReleaseInterval"` // Interval in seconds to release free memory back to OS (0 to disable)
}

func (r *ResourceConfig) ApplyDefaults() {
	if r.Profile == "" {
		r.Profile = "standard"
	}
	switch r.Profile {
	case "low":
		if r.GOGC == 0 {
			r.GOGC = 50
		}
		if r.BufferSize == 0 {
			r.BufferSize = 16
		}
		if r.ConnectionIdle == 0 {
			r.ConnectionIdle = 45
		}
		if r.Handshake == 0 {
			r.Handshake = 4
		}
		if r.UplinkOnly == 0 {
			r.UplinkOnly = 1
		}
		if r.DownlinkOnly == 0 {
			r.DownlinkOnly = 2
		}
		if r.PeriodicMemoryReleaseInterval == 0 {
			r.PeriodicMemoryReleaseInterval = 60
		}
	case "high", "high_performance":
		if r.GOGC == 0 {
			r.GOGC = 100
		}
		if r.BufferSize == 0 {
			r.BufferSize = 128
		}
		if r.ConnectionIdle == 0 {
			r.ConnectionIdle = 120
		}
		if r.Handshake == 0 {
			r.Handshake = 4
		}
		if r.UplinkOnly == 0 {
			r.UplinkOnly = 2
		}
		if r.DownlinkOnly == 0 {
			r.DownlinkOnly = 4
		}
	default: // "standard"
		if r.GOGC == 0 {
			r.GOGC = 80
		}
		if r.BufferSize == 0 {
			r.BufferSize = 32
		}
		if r.ConnectionIdle == 0 {
			r.ConnectionIdle = 60
		}
		if r.Handshake == 0 {
			r.Handshake = 4
		}
		if r.UplinkOnly == 0 {
			r.UplinkOnly = 2
		}
		if r.DownlinkOnly == 0 {
			r.DownlinkOnly = 4
		}
		if r.PeriodicMemoryReleaseInterval == 0 {
			r.PeriodicMemoryReleaseInterval = 120
		}
	}
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
	c := &Conf{
		LogConfig: LogConfig{
			Level:  "info",
			Output: "",
			Access: "none",
		},
		ResourceConfig: ResourceConfig{
			Profile: "standard",
		},
	}
	c.ResourceConfig.ApplyDefaults()
	return c
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
	p.ResourceConfig.ApplyDefaults()
	for i := range p.NodeConfigs {
		if p.NodeConfigs[i].RetryCount == nil {
			p.NodeConfigs[i].RetryCount = intPtr(DefaultNodeRetryCount)
		}
	}
	return nil
}

func intPtr(v int) *int {
	return &v
}
