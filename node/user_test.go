package node

import (
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestCompareUserListRemovesFinalUser(t *testing.T) {
	old := []panel.UserInfo{{Id: 1, Uuid: "last-user"}}
	deleted, added, modified := compareUserList(old, []panel.UserInfo{})
	if len(deleted) != 1 || deleted[0].Uuid != "last-user" {
		t.Fatalf("deleted = %#v, want final user", deleted)
	}
	if len(added) != 0 || len(modified) != 0 {
		t.Fatalf("unexpected additions/modifications: %#v %#v", added, modified)
	}
}
