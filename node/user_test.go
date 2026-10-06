package node

import (
	"reflect"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestOnlineReportThresholdAndMultipleIPs(t *testing.T) {
	online := []panel.OnlineUser{{UID: 1, IP: "192.0.2.1"}, {UID: 1, IP: "192.0.2.2"}, {UID: 2, IP: "192.0.2.3"}}
	traffic := []panel.UserTraffic{{UID: 1, Upload: 1000, Download: 1000}, {UID: 2, Upload: 999}}
	data, count := onlineReport(online, traffic, 1)
	if count != 2 || !reflect.DeepEqual(data, map[int][]string{1: {"192.0.2.1", "192.0.2.2"}}) {
		t.Fatal("online report lost IPs or ignored threshold", data, count)
	}
	if _, count = onlineReport(online, traffic, 0); count != 3 {
		t.Fatal("zero threshold excluded active users")
	}
}

var benchmarkOnlinePayload map[int][]string

func BenchmarkOnlineReportMemory(b *testing.B) {
	online := make([]panel.OnlineUser, 10000)
	for i := range online {
		online[i] = panel.OnlineUser{UID: i/2 + 1, IP: "192.0.2.1"}
	}
	// The previous path copied every entry into an intermediate slice before
	// constructing the identical panel payload. Both paths use the same input.
	b.Run("previous", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var result []panel.OnlineUser
			for _, user := range online {
				result = append(result, user)
			}
			data := make(map[int][]string)
			for _, user := range result {
				data[user.UID] = append(data[user.UID], user.IP)
			}
			benchmarkOnlinePayload = data
		}
	})
	b.Run("current", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkOnlinePayload, _ = onlineReport(online, nil, 0)
		}
	})
}
