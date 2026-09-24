package limiter

import (
	"fmt"
	"sync/atomic"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
)

// Identical workload is run against the untouched upstream and this fork.
func BenchmarkCheckLimit5000Users(b *testing.B) {
	for _, active := range []int{50, 1000, 5000} {
		b.Run(fmt.Sprintf("active-%d", active), func(b *testing.B) {
			Init()
			users := make([]panel.UserInfo, 5000)
			keys := make([]string, len(users))
			for i := range users {
				users[i] = panel.UserInfo{Id: i + 1, Uuid: fmt.Sprintf("user-%d", i), DeviceLimit: 2}
				keys[i] = format.UserTag("bench", users[i].Uuid)
			}
			l := AddLimiter("vless", "bench", users, nil)
			b.Cleanup(func() { DeleteLimiter("bench") })
			for _, key := range keys {
				l.CheckLimit(key, "192.0.2.1", true)
			}
			var next atomic.Uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					key := keys[int(next.Add(1)-1)%active]
					if _, reject := l.CheckLimit(key, "192.0.2.1", true); reject {
						b.Error("known IP rejected")
						return
					}
				}
			})
		})
	}
}
