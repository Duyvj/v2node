package panel

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
	"github.com/wyx2685/v2node/conf"
)

func newTestPanelClient(t *testing.T, serverURL string, runtimeConfig conf.RuntimeConfig) *Client {
	t.Helper()
	client, err := New(&conf.NodeConfig{APIHost: serverURL, NodeID: 1, Key: "key"}, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func newStaticUserListClient(
	t *testing.T,
	status int,
	contentType string,
	etag string,
	body []byte,
	runtimeConfig conf.RuntimeConfig,
) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", contentType)
		if etag != "" {
			writer.Header().Set("ETag", etag)
		}
		writer.WriteHeader(status)
		_, _ = writer.Write(body)
	}))
	t.Cleanup(server.Close)
	return newTestPanelClient(t, server.URL, runtimeConfig)
}

func TestJSONUserListDrainsTrailingFieldsAndReusesConnection(t *testing.T) {
	var connections atomic.Int32
	padding := strings.Repeat("x", 64*1024)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"users":[{"id":1,"uuid":"user"}],"padding":%q}`, padding)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	client := newTestPanelClient(t, server.URL, conf.RuntimeConfig{MaxPanelResponseBytes: 1024 * 1024, MaxUsers: 10})
	for i := 0; i < 2; i++ {
		users, err := client.GetUserList(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != 1 || users[0].Uuid != "user" {
			t.Fatalf("unexpected users: %#v", users)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("HTTP connections = %d, want one reused connection", got)
	}
}

func TestHTTP200EmptyUserListIsDistinctFromNotModified(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        []byte
	}{
		{name: "JSON", contentType: "application/json", body: []byte(`{"users":[]}`)},
		{name: "MessagePack", contentType: "application/x-msgpack", body: []byte{0x81, 0xa5, 'u', 's', 'e', 'r', 's', 0x90}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newStaticUserListClient(t, http.StatusOK, test.contentType, `"valid"`, test.body, conf.RuntimeConfig{})
			users, err := client.GetUserList(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if users == nil || len(users) != 0 {
				t.Fatalf("HTTP 200 empty list = %#v, want non-nil empty slice", users)
			}
			if client.userEtag != `"valid"` {
				t.Fatalf("saved ETag = %q, want %q", client.userEtag, `"valid"`)
			}
		})
	}
}

func TestMsgpackUserListDecodesUsers(t *testing.T) {
	body, err := msgpack.Marshal(UserListBody{Users: []UserInfo{{
		Id:          7,
		Uuid:        "user-7",
		SpeedLimit:  1024,
		DeviceLimit: 2,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	client := newStaticUserListClient(t, http.StatusOK, "application/x-msgpack", "", body, conf.RuntimeConfig{})
	users, err := client.GetUserList(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Id != 7 || users[0].Uuid != "user-7" ||
		users[0].SpeedLimit != 1024 || users[0].DeviceLimit != 2 {
		t.Fatalf("unexpected MessagePack users: %#v", users)
	}
}

func TestJSONUserListRequiresExactlyOneTopLevelUsersField(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing", body: `{}`},
		{name: "nested only", body: `{"metadata":{"users":[]}}`},
		{name: "duplicate", body: `{"users":[],"users":[]}`},
		{name: "null", body: `{"users":null}`},
		{name: "wrong top level", body: `[]`},
		{name: "truncated", body: `{"users":[`},
		{name: "trailing value", body: `{"users":[]} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newStaticUserListClient(
				t,
				http.StatusOK,
				"application/json",
				`"invalid"`,
				[]byte(test.body),
				conf.RuntimeConfig{},
			)
			client.userEtag = `"previous"`
			if _, err := client.GetUserList(t.Context()); err == nil {
				t.Fatal("invalid JSON user envelope was accepted")
			}
			if client.userEtag != `"previous"` {
				t.Fatalf("ETag changed after invalid response: %q", client.userEtag)
			}
		})
	}
}

func TestMsgpackUserListRequiresExactlyOneTopLevelUsersField(t *testing.T) {
	nestedOnly, err := msgpack.Marshal(map[string]any{
		"metadata": map[string]any{"users": []UserInfo{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	validEmpty := []byte{0x81, 0xa5, 'u', 's', 'e', 'r', 's', 0x90}
	tests := []struct {
		name string
		body []byte
	}{
		{name: "missing", body: []byte{0x80}},
		{name: "nested only", body: nestedOnly},
		{name: "duplicate", body: []byte{0x82, 0xa5, 'u', 's', 'e', 'r', 's', 0x90, 0xa5, 'u', 's', 'e', 'r', 's', 0x90}},
		{name: "nil", body: []byte{0x81, 0xa5, 'u', 's', 'e', 'r', 's', 0xc0}},
		{name: "wrong top level", body: []byte{0x90}},
		{name: "truncated", body: []byte{0x81, 0xa5, 'u', 's', 'e', 'r', 's', 0x91}},
		{name: "trailing value", body: append(append([]byte(nil), validEmpty...), 0xc0)},
		{name: "truncated trailing value", body: append(append([]byte(nil), validEmpty...), 0xd9)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newStaticUserListClient(
				t,
				http.StatusOK,
				"application/x-msgpack",
				`"invalid"`,
				test.body,
				conf.RuntimeConfig{},
			)
			client.userEtag = `"previous"`
			if _, err := client.GetUserList(t.Context()); err == nil {
				t.Fatal("invalid MessagePack user envelope was accepted")
			}
			if client.userEtag != `"previous"` {
				t.Fatalf("ETag changed after invalid response: %q", client.userEtag)
			}
		})
	}
}

func TestUserListRejectsNonSuccessStatusWithoutSavingETag(t *testing.T) {
	var requestETag atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestETag.Store(request.Header.Get("If-None-Match"))
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("ETag", `"error"`)
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"users":[]}`))
	}))
	defer server.Close()

	client := newTestPanelClient(t, server.URL, conf.RuntimeConfig{})
	client.userEtag = `"previous"`
	if _, err := client.GetUserList(t.Context()); err == nil || !strings.Contains(err.Error(), "HTTP status 500") {
		t.Fatalf("non-success response error = %v", err)
	}
	if got := requestETag.Load(); got != `"previous"` {
		t.Fatalf("If-None-Match = %q, want previous valid ETag", got)
	}
	if client.userEtag != `"previous"` {
		t.Fatalf("ETag changed after non-success response: %q", client.userEtag)
	}
}

func TestUserListAcceptsNotModifiedWithoutReplacingETag(t *testing.T) {
	client := newStaticUserListClient(t, http.StatusNotModified, "application/json", `"ignored"`, nil, conf.RuntimeConfig{})
	client.userEtag = `"previous"`
	users, err := client.GetUserList(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if users != nil {
		t.Fatalf("HTTP 304 users = %#v, want nil", users)
	}
	if client.userEtag != `"previous"` {
		t.Fatalf("ETag changed after HTTP 304: %q", client.userEtag)
	}
}

func TestMsgpackClaimedUserCountIsRejectedBeforeAllocation(t *testing.T) {
	// {"users": array32(100001)} with no elements. The count must be rejected
	// before make([]UserInfo, count) can allocate panel-controlled memory.
	body := []byte{0x81, 0xa5, 'u', 's', 'e', 'r', 's', 0xdd, 0x00, 0x01, 0x86, 0xa1}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/x-msgpack")
		_, _ = writer.Write(body)
	}))
	defer server.Close()
	client := newTestPanelClient(t, server.URL, conf.RuntimeConfig{MaxPanelResponseBytes: 1024 * 1024, MaxUsers: 10})
	if _, err := client.GetUserList(t.Context()); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("oversized msgpack array error = %v", err)
	}
}

func TestUserListByteLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Content-Length", "2097152")
		_, _ = writer.Write([]byte(`{"users":[]}`))
	}))
	defer server.Close()
	client := newTestPanelClient(t, server.URL, conf.RuntimeConfig{MaxPanelResponseBytes: 1024 * 1024, MaxUsers: 10})
	if _, err := client.GetUserList(t.Context()); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestChunkedUserListByteLimit(t *testing.T) {
	body := append([]byte(`{"users":[]}`), []byte(strings.Repeat(" ", 70*1024))...)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("ETag", `"oversized"`)
		writer.(http.Flusher).Flush()
		_, _ = writer.Write(body)
	}))
	defer server.Close()

	client := newTestPanelClient(t, server.URL, conf.RuntimeConfig{MaxPanelResponseBytes: 64 * 1024, MaxUsers: 10})
	client.userEtag = `"previous"`
	if _, err := client.GetUserList(t.Context()); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("oversized chunked response error = %v", err)
	}
	if client.userEtag != `"previous"` {
		t.Fatalf("ETag changed after oversized response: %q", client.userEtag)
	}
}
