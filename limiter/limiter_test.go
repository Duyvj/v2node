package limiter

import (
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
)

func TestSetAliveListAffectsDeviceLimit(t *testing.T) {
	Init()
	const (
		tag  = "node-a"
		uuid = "user-a"
	)
	l := AddLimiter("vmess", tag, []panel.UserInfo{{Id: 7, Uuid: uuid, DeviceLimit: 1}}, map[int]int{7: 0})
	key := format.UserTag(tag, uuid)

	if _, rejected := l.CheckLimit(key, "192.0.2.1", true); rejected {
		t.Fatal("first IP was rejected below the device limit")
	}
	l.SetAliveList(map[int]int{7: 1})
	if _, rejected := l.CheckLimit(key, "192.0.2.2", true); !rejected {
		t.Fatal("new IP was accepted after the device limit was reached")
	}
}
