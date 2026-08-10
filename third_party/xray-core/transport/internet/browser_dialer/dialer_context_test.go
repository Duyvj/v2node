package browser_dialer

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestConnectionQueueDoesNotBlockWhenFull(t *testing.T) {
	generation := newConnectionGeneration(1)
	defer generation.retire()
	if !generation.enqueue(nil) {
		t.Fatal("first connection was unexpectedly rejected")
	}

	result := make(chan bool, 1)
	go func() {
		result <- generation.enqueue(nil)
	}()
	select {
	case accepted := <-result:
		if accepted {
			t.Fatal("full connection queue accepted another connection")
		}
	case <-time.After(time.Second):
		t.Fatal("full connection queue blocked the HTTP handler")
	}
}

func TestQueueFullClosesRejectedConnection(t *testing.T) {
	generation := newConnectionGeneration(1)
	defer generation.retire()
	if !generation.enqueue(nil) {
		t.Fatal("failed to fill connection queue")
	}

	serverConn, browserConn := websocketPair(t)
	if got := enqueueBrowserConnection(generation, serverConn); got != enqueueQueueFull {
		t.Fatalf("full connection queue result = %v, want queue full", got)
	}
	assertWebSocketClosed(t, browserConn)
}

func TestQueueFullWarningsAreAggregated(t *testing.T) {
	generation := newConnectionGeneration(1)
	defer generation.retire()
	start := time.Unix(1_700_000_000, 0)

	if suppressed, report := generation.recordQueueFull(start); !report || suppressed != 0 {
		t.Fatalf("first queue-full event = (%d, %v), want (0, true)", suppressed, report)
	}
	for i := 0; i < 1_000; i++ {
		if suppressed, report := generation.recordQueueFull(start.Add(time.Second)); report || suppressed != 0 {
			t.Fatalf("queue-full event %d inside window = (%d, %v), want (0, false)", i, suppressed, report)
		}
	}
	if suppressed, report := generation.recordQueueFull(start.Add(queueFullWarningInterval)); !report || suppressed != 1_000 {
		t.Fatalf("next queue-full report = (%d, %v), want (1000, true)", suppressed, report)
	}
	if suppressed, report := generation.recordQueueFull(start.Add(queueFullWarningInterval + time.Second)); report || suppressed != 0 {
		t.Fatalf("event after aggregate report = (%d, %v), want (0, false)", suppressed, report)
	}
}

func TestRetiredGenerationRejectsAndClosesLateConnection(t *testing.T) {
	generation := newConnectionGeneration(1)
	generation.retire()

	serverConn, browserConn := websocketPair(t)
	if got := enqueueBrowserConnection(generation, serverConn); got != enqueueGenerationRetired {
		t.Fatalf("retired generation enqueue result = %v, want retired", got)
	}
	assertWebSocketClosed(t, browserConn)
	if got := len(generation.queue); got != 0 {
		t.Fatalf("retired generation retained %d late connections", got)
	}
}

func TestStaleCSRFTokenCanRecoverWithoutReload(t *testing.T) {
	const currentToken = "current-process-token"

	generation := newConnectionGeneration(1)
	defer generation.retire()
	if got := bytes.Count(webpage, []byte("csrfToken")); got != 1 {
		t.Fatalf("browser page token placeholder count = %d, want 1", got)
	}
	page := bytes.ReplaceAll(webpage, []byte("csrfToken"), []byte(currentToken))
	testServer := httptest.NewServer(browserHandler(generation, currentToken, page))
	defer testServer.Close()
	sameOriginHeader := http.Header{"Origin": []string{testServer.URL}}

	pageRequest, err := http.NewRequest(http.MethodGet, testServer.URL, nil)
	if err != nil {
		t.Fatalf("create same-origin page request: %v", err)
	}
	pageRequest.Header.Set("Origin", testServer.URL)
	pageResponse, err := http.DefaultClient.Do(pageRequest)
	if err != nil {
		t.Fatalf("fetch same-origin browser page: %v", err)
	}
	servedPage, err := io.ReadAll(pageResponse.Body)
	_ = pageResponse.Body.Close()
	if err != nil {
		t.Fatalf("read same-origin browser page: %v", err)
	}
	if pageResponse.StatusCode != http.StatusOK || !bytes.Equal(servedPage, page) {
		t.Fatalf("same-origin browser page status/body = %d/%t, want 200/true", pageResponse.StatusCode, bytes.Equal(servedPage, page))
	}

	websocketBaseURL := "ws" + strings.TrimPrefix(testServer.URL, "http") + "/websocket?token="
	staleConn, response, err := websocket.DefaultDialer.Dial(websocketBaseURL+"previous-process-token", sameOriginHeader)
	if staleConn != nil {
		_ = staleConn.Close()
		t.Fatal("stale browser dialer token unexpectedly upgraded")
	}
	if err == nil {
		t.Fatal("stale browser dialer token unexpectedly succeeded")
	}
	if response == nil {
		t.Fatal("stale token rejection did not return an HTTP response")
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("stale token status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	tokenRequest, err := http.NewRequest(http.MethodGet, testServer.URL+csrfTokenPath, nil)
	if err != nil {
		t.Fatalf("create same-origin token request: %v", err)
	}
	tokenRequest.Header.Set("Origin", testServer.URL)
	tokenResponse, err := http.DefaultClient.Do(tokenRequest)
	if err != nil {
		t.Fatalf("refresh browser dialer token: %v", err)
	}
	defer tokenResponse.Body.Close()
	refreshedToken, err := io.ReadAll(tokenResponse.Body)
	if err != nil {
		t.Fatalf("read refreshed browser dialer token: %v", err)
	}
	if string(refreshedToken) != currentToken {
		t.Fatalf("refreshed token = %q, want %q", refreshedToken, currentToken)
	}
	if got := tokenResponse.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("token Cache-Control = %q, want no-store", got)
	}

	browserConn, _, err := websocket.DefaultDialer.Dial(websocketBaseURL+string(refreshedToken), sameOriginHeader)
	if err != nil {
		t.Fatalf("dial with refreshed browser token: %v", err)
	}
	defer browserConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	serverConn, err := generation.next(ctx)
	if err != nil {
		t.Fatalf("take connection authenticated by refreshed token: %v", err)
	}
	defer serverConn.Close()

	if !bytes.Contains(page, []byte(`fetch(tokenEndpoint, {cache: "no-store", signal: controller.signal})`)) ||
		bytes.Count(page, []byte("recoverConnection();")) < 3 {
		t.Fatal("browser page does not automatically fetch a replacement token before reconnecting")
	}
}

func TestBrowserHandlerRejectsCrossOriginTokenDisclosureAndUpgrade(t *testing.T) {
	const currentToken = "current-process-token"
	const hostileOrigin = "https://attacker.example"

	generation := newConnectionGeneration(1)
	defer generation.retire()
	page := bytes.ReplaceAll(webpage, []byte("csrfToken"), []byte(currentToken))
	testServer := httptest.NewServer(browserHandler(generation, currentToken, page))
	defer testServer.Close()

	for _, path := range []string{"/", csrfTokenPath} {
		request, err := http.NewRequest(http.MethodGet, testServer.URL+path, nil)
		if err != nil {
			t.Fatalf("create hostile request for %s: %v", path, err)
		}
		request.Header.Set("Origin", hostileOrigin)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("send hostile request for %s: %v", path, err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatalf("read hostile response for %s: %v", path, readErr)
		}
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("hostile request for %s status = %d, want %d", path, response.StatusCode, http.StatusForbidden)
		}
		if got := response.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("hostile request for %s exposed CORS origin %q", path, got)
		}
		if bytes.Contains(body, []byte(currentToken)) {
			t.Errorf("hostile request for %s disclosed browser token", path)
		}
	}

	websocketURL := "ws" + strings.TrimPrefix(testServer.URL, "http") + "/websocket?token=" + currentToken
	conn, response, err := websocket.DefaultDialer.Dial(websocketURL, http.Header{"Origin": []string{hostileOrigin}})
	if conn != nil {
		_ = conn.Close()
		t.Fatal("hostile WebSocket origin unexpectedly upgraded")
	}
	if err == nil || response == nil {
		t.Fatalf("hostile WebSocket upgrade error/response = %v/%v, want rejection response", err, response)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("hostile WebSocket status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	if got := len(generation.queue); got != 0 {
		t.Fatalf("hostile WebSocket enqueued %d connections", got)
	}
}

func TestBrowserOriginMatchesHostAndScheme(t *testing.T) {
	tests := []struct {
		name   string
		url    string
		origin string
		want   bool
	}{
		{name: "empty non-browser origin", url: "http://dialer.example:8080/websocket", want: true},
		{name: "same HTTP origin", url: "http://dialer.example:8080/websocket", origin: "http://dialer.example:8080", want: true},
		{name: "same default HTTP port", url: "http://dialer.example/websocket", origin: "http://dialer.example:80", want: true},
		{name: "wrong host", url: "http://dialer.example:8080/websocket", origin: "http://attacker.example:8080", want: false},
		{name: "wrong port", url: "http://dialer.example:8080/websocket", origin: "http://dialer.example:8081", want: false},
		{name: "wrong scheme", url: "http://dialer.example:8080/websocket", origin: "https://dialer.example:8080", want: false},
		{name: "same HTTPS origin", url: "https://dialer.example/websocket", origin: "https://dialer.example", want: true},
		{name: "opaque browser origin", url: "http://dialer.example/websocket", origin: "null", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.url, nil)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if got := hasSameBrowserOrigin(request); got != test.want {
				t.Fatalf("hasSameBrowserOrigin() = %v, want %v", got, test.want)
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet, "http://dialer.example/websocket", nil)
	request.Header.Add("Origin", "http://dialer.example")
	request.Header.Add("Origin", "https://attacker.example")
	if hasSameBrowserOrigin(request) {
		t.Fatal("duplicate Origin headers were accepted")
	}
}

func TestDialWSContextCancelsWhileWaitingForBrowser(t *testing.T) {
	resetBrowserDialer(t)
	generation := newConnectionGeneration(1)
	activeGeneration.Store(generation)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := DialWSContext(ctx, "wss://example.invalid/", nil)
		result <- err
	}()
	timer := time.AfterFunc(10*time.Millisecond, cancel)
	defer timer.Stop()

	select {
	case err := <-result:
		if !stderrors.Is(err, context.Canceled) {
			t.Fatalf("DialWSContext error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled browser dial remained blocked waiting for a connection")
	}
}

func TestReloadRetiresGenerationAndWakesWaiters(t *testing.T) {
	resetBrowserDialer(t)
	if err := reload(unusedTCPAddress(t)); err != nil {
		t.Fatalf("start first browser dialer generation: %v", err)
	}
	oldGeneration := activeGeneration.Load()
	waiter := make(chan error, 1)
	go func() {
		_, err := oldGeneration.next(context.Background())
		waiter <- err
	}()

	if err := reload(unusedTCPAddress(t)); err != nil {
		t.Fatalf("start replacement browser dialer generation: %v", err)
	}
	if activeGeneration.Load() == oldGeneration {
		t.Fatal("reload did not publish a replacement generation")
	}
	select {
	case err := <-waiter:
		if !stderrors.Is(err, errGenerationRetired) {
			t.Fatalf("old-generation waiter error = %v, want retirement", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reload did not wake old-generation waiter")
	}
}

func TestDialWSContextCancelsAcknowledgmentRead(t *testing.T) {
	resetBrowserDialer(t)
	generation := newConnectionGeneration(1)
	activeGeneration.Store(generation)
	serverConn, browserConn := websocketPair(t)
	if !generation.enqueue(serverConn) {
		t.Fatal("failed to enqueue browser connection")
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := DialWSContext(ctx, "wss://example.invalid/", nil)
		result <- err
	}()

	if _, _, err := browserConn.ReadMessage(); err != nil {
		t.Fatalf("browser failed to read task: %v", err)
	}
	cancel()
	select {
	case err := <-result:
		if !stderrors.Is(err, context.Canceled) {
			t.Fatalf("DialWSContext error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled browser dial remained blocked reading acknowledgment")
	}
}

func TestDialPacketContextCancelsResponseAcknowledgment(t *testing.T) {
	resetBrowserDialer(t)
	generation := newConnectionGeneration(1)
	activeGeneration.Store(generation)
	serverConn, browserConn := websocketPair(t)
	if !generation.enqueue(serverConn) {
		t.Fatal("failed to enqueue browser connection")
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- DialPacketContext(ctx, http.MethodPost, "https://example.invalid/", http.Header{}, nil, []byte("payload"))
	}()

	if _, _, err := browserConn.ReadMessage(); err != nil {
		t.Fatalf("browser failed to read packet task: %v", err)
	}
	if err := browserConn.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
		t.Fatalf("browser failed to acknowledge packet task: %v", err)
	}
	if _, payload, err := browserConn.ReadMessage(); err != nil {
		t.Fatalf("browser failed to read packet payload: %v", err)
	} else if string(payload) != "payload" {
		t.Fatalf("packet payload = %q, want payload", payload)
	}

	cancel()
	select {
	case err := <-result:
		if !stderrors.Is(err, context.Canceled) {
			t.Fatalf("DialPacketContext error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled packet dial remained blocked reading response acknowledgment")
	}
}

func TestDialWaiterMovesToReplacementGeneration(t *testing.T) {
	resetBrowserDialer(t)
	oldGeneration := newConnectionGeneration(1)
	activeGeneration.Store(oldGeneration)
	oldServerConn, oldBrowserConn := websocketPair(t)
	if !oldGeneration.enqueue(oldServerConn) {
		t.Fatal("old generation rejected browser connection")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan struct {
		conn *websocket.Conn
		err  error
	}, 1)
	go func() {
		conn, err := DialWSContext(ctx, "wss://example.invalid/", nil)
		result <- struct {
			conn *websocket.Conn
			err  error
		}{conn: conn, err: err}
	}()
	if _, _, err := oldBrowserConn.ReadMessage(); err != nil {
		t.Fatalf("browser failed to read task from old generation: %v", err)
	}

	newGeneration := newConnectionGeneration(1)
	activeGeneration.Store(newGeneration)
	oldGeneration.retire()
	assertWebSocketClosed(t, oldBrowserConn)

	serverConn, browserConn := websocketPair(t)
	if !newGeneration.enqueue(serverConn) {
		t.Fatal("replacement generation rejected browser connection")
	}
	if _, _, err := browserConn.ReadMessage(); err != nil {
		t.Fatalf("browser failed to read task from replacement generation: %v", err)
	}
	if err := browserConn.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
		t.Fatalf("browser failed to acknowledge task: %v", err)
	}

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("dial through replacement generation failed: %v", got.err)
		}
		if got.conn != serverConn {
			t.Fatal("dial returned a connection outside the replacement generation")
		}
	case <-ctx.Done():
		t.Fatal("old-generation waiter was not awakened by replacement")
	}
}

func TestReloadBindFailureDisablesBrowserDialer(t *testing.T) {
	resetBrowserDialer(t)
	firstAddr := unusedTCPAddress(t)
	if err := reload(firstAddr); err != nil {
		t.Fatalf("start browser dialer: %v", err)
	}
	if !HasBrowserDialer() {
		t.Fatal("browser dialer was not enabled after a successful bind")
	}
	oldGeneration := activeGeneration.Load()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve occupied address: %v", err)
	}
	defer occupied.Close()
	if err := reload(occupied.Addr().String()); err == nil {
		t.Fatal("reload unexpectedly succeeded on an occupied address")
	}
	if HasBrowserDialer() {
		t.Fatal("browser dialing remained enabled after bind failure")
	}
	select {
	case <-oldGeneration.done:
	default:
		t.Fatal("bind failure did not wake the old generation")
	}
}

func TestServeFailureDisablesPublishedGeneration(t *testing.T) {
	resetBrowserDialer(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	generation := newConnectionGeneration(1)
	httpServer := &http.Server{}
	mu.Lock()
	server = httpServer
	currentAddr = listener.Addr().String()
	activeGeneration.Store(generation)
	mu.Unlock()

	serve(httpServer, listener, generation)
	if HasBrowserDialer() {
		t.Fatal("browser dialing remained enabled after serving stopped")
	}
	select {
	case <-generation.done:
	default:
		t.Fatal("serve failure did not wake generation waiters")
	}
}

func resetBrowserDialer(t *testing.T) {
	t.Helper()
	if err := reload(""); err != nil {
		t.Fatalf("reset browser dialer: %v", err)
	}
	t.Cleanup(func() {
		if err := reload(""); err != nil {
			t.Errorf("cleanup browser dialer: %v", err)
		}
	})
}

func unusedTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find unused address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release unused address: %v", err)
	}
	return addr
}

func websocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	serverConnections := make(chan *websocket.Conn, 1)
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConnections <- conn
	}))
	t.Cleanup(testServer.Close)

	url := "ws" + strings.TrimPrefix(testServer.URL, "http")
	browserConn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial test websocket: %v", err)
	}
	serverConn := <-serverConnections
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = browserConn.Close()
	})
	return serverConn, browserConn
}

func assertWebSocketClosed(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("rejected browser connection remained open")
	}
}
