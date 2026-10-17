package limiter

import (
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
)

const testTag = "test-inbound"

func newTestLimiter(t *testing.T, deviceLimit int) (*Limiter, string) {
	t.Helper()
	Init()
	uuid := "00000000-0000-4000-8000-000000000001"
	l := AddLimiter("vless", testTag, []panel.UserInfo{{Id: 1, Uuid: uuid, DeviceLimit: deviceLimit}}, map[int]int{})
	t.Cleanup(func() { DeleteLimiter(testTag) })
	return l, format.UserTag(testTag, uuid)
}

func TestCheckLimitDoesNotAllocateForAKnownDevice(t *testing.T) {
	l, user := newTestLimiter(t, 0)
	if _, reject := l.CheckLimit(user, "1.2.3.4", true); reject {
		t.Fatal("first connection rejected")
	}
	allocs := testing.AllocsPerRun(200, func() {
		l.CheckLimit(user, "1.2.3.4", true)
	})
	if allocs > 0 {
		t.Fatalf("CheckLimit allocates %.1f times per connection from an already online device", allocs)
	}
}

func TestCheckLimitStillCountsDevices(t *testing.T) {
	l, user := newTestLimiter(t, 1)
	l.AliveList[1] = 1
	if _, reject := l.CheckLimit(user, "1.2.3.4", true); !reject {
		t.Fatal("device over the limit from alive list was accepted")
	}
	l.AliveList[1] = 0
	if _, reject := l.CheckLimit(user, "1.2.3.4", true); reject {
		t.Fatal("first device rejected")
	}
	if _, reject := l.CheckLimit(user, "1.2.3.4", true); reject {
		t.Fatal("same device rejected on its second connection")
	}
	online, _ := l.GetOnlineDevice()
	if len(*online) != 1 || (*online)[0].IP != "1.2.3.4" || (*online)[0].UID != 1 {
		t.Fatalf("online devices = %+v", *online)
	}
}
