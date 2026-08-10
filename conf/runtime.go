package conf

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// RuntimeConfig contains local safeguards that do not change panel semantics.
// All fields are optional in config.json; Normalize supplies conservative defaults.
type RuntimeConfig struct {
	// Clamp panel-provided polling intervals to prevent accidental busy loops.
	MinPollIntervalSeconds int `mapstructure:"MinPollIntervalSeconds"`
	MaxPollIntervalSeconds int `mapstructure:"MaxPollIntervalSeconds"`
	// Xray policy buffer per connection, in KiB. 64 KiB is a moderate small-VPS default.
	BufferSizeKB int `mapstructure:"BufferSizeKB"`
	// Optional Go runtime memory limit, e.g. "448MiB". Empty leaves GOMEMLIMIT/default intact.
	MemoryLimit string `mapstructure:"MemoryLimit"`
	// Bound the transient online-IP set. Unlimited users continue to connect
	// after a cap is reached, but excess IPs are not retained for reporting.
	MaxTrackedIPsPerUser int `mapstructure:"MaxTrackedIPsPerUser"`
	MaxTrackedIPsPerNode int `mapstructure:"MaxTrackedIPsPerNode"`
	// Bound panel-controlled response allocations, including streamed user lists.
	MaxPanelResponseBytes int `mapstructure:"MaxPanelResponseBytes"`
	MaxUsers              int `mapstructure:"MaxUsers"`
}

func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		MinPollIntervalSeconds: 30,
		MaxPollIntervalSeconds: 3600,
		BufferSizeKB:           64,
		MaxTrackedIPsPerUser:   256,
		MaxTrackedIPsPerNode:   32768,
		MaxPanelResponseBytes:  16 * 1024 * 1024,
		MaxUsers:               100000,
	}
}

func (r *RuntimeConfig) Normalize() {
	defaults := DefaultRuntimeConfig()
	if r.MinPollIntervalSeconds < 5 {
		r.MinPollIntervalSeconds = defaults.MinPollIntervalSeconds
	}
	if r.MinPollIntervalSeconds > 24*60*60 {
		r.MinPollIntervalSeconds = 24 * 60 * 60
	}
	if r.MaxPollIntervalSeconds < r.MinPollIntervalSeconds {
		r.MaxPollIntervalSeconds = defaults.MaxPollIntervalSeconds
		if r.MaxPollIntervalSeconds < r.MinPollIntervalSeconds {
			r.MaxPollIntervalSeconds = r.MinPollIntervalSeconds
		}
	}
	if r.MaxPollIntervalSeconds > 24*60*60 {
		r.MaxPollIntervalSeconds = 24 * 60 * 60
	}
	if r.BufferSizeKB <= 0 {
		r.BufferSizeKB = defaults.BufferSizeKB
	}
	if r.BufferSizeKB > 512 {
		r.BufferSizeKB = 512
	}
	if r.MaxTrackedIPsPerUser <= 0 {
		r.MaxTrackedIPsPerUser = defaults.MaxTrackedIPsPerUser
	}
	if r.MaxTrackedIPsPerUser > 65536 {
		r.MaxTrackedIPsPerUser = 65536
	}
	if r.MaxTrackedIPsPerNode <= 0 {
		r.MaxTrackedIPsPerNode = defaults.MaxTrackedIPsPerNode
	}
	if r.MaxTrackedIPsPerNode < r.MaxTrackedIPsPerUser {
		r.MaxTrackedIPsPerNode = r.MaxTrackedIPsPerUser
	}
	if r.MaxTrackedIPsPerNode > 1_000_000 {
		r.MaxTrackedIPsPerNode = 1_000_000
	}
	if r.MaxPanelResponseBytes <= 0 {
		r.MaxPanelResponseBytes = defaults.MaxPanelResponseBytes
	}
	if r.MaxPanelResponseBytes < 64*1024 {
		r.MaxPanelResponseBytes = 64 * 1024
	}
	if r.MaxPanelResponseBytes > 256*1024*1024 {
		r.MaxPanelResponseBytes = 256 * 1024 * 1024
	}
	if r.MaxUsers <= 0 {
		r.MaxUsers = defaults.MaxUsers
	}
	if r.MaxUsers > 1_000_000 {
		r.MaxUsers = 1_000_000
	}
	if r.MemoryLimit != "" {
		r.MemoryLimit = strings.TrimSpace(r.MemoryLimit)
	}
}

func (r RuntimeConfig) ClampPollInterval(interval time.Duration) time.Duration {
	r.Normalize()
	minInterval := time.Duration(r.MinPollIntervalSeconds) * time.Second
	maxInterval := time.Duration(r.MaxPollIntervalSeconds) * time.Second
	if interval < minInterval {
		return minInterval
	}
	if interval > maxInterval {
		return maxInterval
	}
	return interval
}

// ParseMemoryLimit accepts bytes or binary/decimal suffixes used in deployment files.
func ParseMemoryLimit(value string) (int64, error) {
	v := strings.TrimSpace(strings.ToUpper(value))
	if v == "" || v == "0" || v == "OFF" || v == "NONE" {
		return 0, nil
	}
	units := []struct {
		suffix string
		factor float64
	}{
		{"KIB", 1024}, {"MIB", 1024 * 1024}, {"GIB", 1024 * 1024 * 1024},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
	}
	factor := float64(1)
	number := v
	for _, unit := range units {
		if strings.HasSuffix(v, unit.suffix) {
			factor = unit.factor
			number = strings.TrimSpace(strings.TrimSuffix(v, unit.suffix))
			break
		}
	}
	n, err := strconv.ParseFloat(number, 64)
	if err != nil || n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("invalid memory limit %q", value)
	}
	bytes := n * factor
	if bytes > math.MaxInt64 {
		return 0, fmt.Errorf("memory limit %q is too large", value)
	}
	return int64(bytes), nil
}
