package panel

import (
	"context"
	"fmt"
	"github.com/wyx2685/v2node/conf"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c, err := New(&conf.NodeConfig{APIHost: s.URL, NodeID: 1, Key: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func TestEmptyUsersRevokeAnd304ReplaysSnapshot(t *testing.T) {
	var phase atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch phase.Load() {
		case 0:
			w.Header().Set("ETag", "a")
			fmt.Fprint(w, `{"users":[{"id":1,"uuid":"a","device_limit":2}]}`)
		case 1:
			w.WriteHeader(304)
		case 2:
			fmt.Fprint(w, `{"users":[]}`)
		}
	})
	users, err := c.GetUserList(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatal(users, err)
	}
	phase.Store(1)
	users, err = c.GetUserList(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatal("304 lost snapshot", err)
	}
	phase.Store(2)
	users, err = c.GetUserList(context.Background())
	if err != nil || users == nil || len(users) != 0 {
		t.Fatal("empty list mistaken for no update", err)
	}
}
func TestFailedReportsDoNotAcknowledge(t *testing.T) {
	var requests atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) })
	if c.ReportUserTraffic(context.Background(), []UserTraffic{{UID: 1, Upload: 10}}) == nil {
		t.Fatal("HTTP 500 acknowledged")
	}
	if requests.Load() != 1 {
		t.Fatal("traffic POST retried")
	}
	data := map[int][]string{1: {"192.0.2.1"}}
	if c.ReportNodeOnlineUsers(context.Background(), &data) == nil {
		t.Fatal("online failure acknowledged")
	}
}
func TestOversizedPanelBodyIsBounded(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat(" ", maxPanelBody+1)) })
	if _, err := c.GetUserList(context.Background()); err == nil {
		t.Fatal("oversized users accepted")
	}
}
func TestMsgpackDeclaredArrayCannotAllocateUnboundedMemory(t *testing.T) {
	// {users: array32(2^31-1)} with no elements.
	data := append([]byte{0x81, 0xa5}, []byte("users")...)
	data = append(data, 0xdd, 0x7f, 0xff, 0xff, 0xff)
	if _, err := decodeUsersMsgpack(data); err == nil {
		t.Fatal("unbounded array declaration accepted")
	}
}
func TestInvalidNodeResponseDoesNotPoisonETag(t *testing.T) {
	var requests atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", "bad")
		fmt.Fprint(w, `{"protocol":"invalid"}`)
	})
	for i := 0; i < 2; i++ {
		if _, err := c.GetNodeInfo(context.Background()); err == nil {
			t.Fatal("invalid configuration cached as success")
		}
	}
	if c.nodeEtag != "" || c.responseBodyHash != "" {
		t.Fatal("invalid state published")
	}
}
func TestInvalidIntervalsUseSafeDefault(t *testing.T) {
	for _, v := range []any{nil, -1, "invalid", 0, 1e30} {
		if intervalToTime(v).Seconds() != 60 {
			t.Fatalf("unsafe interval %v", v)
		}
	}
}
