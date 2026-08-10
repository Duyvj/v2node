package browser_dialer

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/common/uuid"
)

//go:embed dialer.html
var webpage []byte

type task struct {
	Method         string `json:"method"`
	URL            string `json:"url"`
	Extra          any    `json:"extra,omitempty"`
	StreamResponse bool   `json:"streamResponse"`
}

var (
	activeGeneration atomic.Pointer[connectionGeneration]
	server           *http.Server
	currentAddr      string
	mu               sync.Mutex
)

const (
	connectionQueueSize      = 256
	csrfTokenPath            = "/csrf-token"
	queueFullWarningInterval = 30 * time.Second
)

var errGenerationRetired = stderrors.New("browser dialer generation retired")

type enqueueResult uint8

const (
	enqueueAccepted enqueueResult = iota
	enqueueQueueFull
	enqueueGenerationRetired
)

type connectionGeneration struct {
	queue    chan *websocket.Conn
	lifetime context.Context
	done     <-chan struct{}
	cancel   context.CancelFunc

	mu      sync.Mutex
	retired bool

	warningMu                   sync.Mutex
	lastQueueFullWarning        time.Time
	suppressedQueueFullWarnings uint64
}

func newConnectionGeneration(queueSize int) *connectionGeneration {
	ctx, cancel := context.WithCancel(context.Background())
	return &connectionGeneration{
		queue:    make(chan *websocket.Conn, queueSize),
		lifetime: ctx,
		done:     ctx.Done(),
		cancel:   cancel,
	}
}

// tryEnqueue is synchronized with retire so a connection can never be added
// after the retirement drain has completed.
func (g *connectionGeneration) tryEnqueue(conn *websocket.Conn) enqueueResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired {
		return enqueueGenerationRetired
	}

	select {
	case g.queue <- conn:
		return enqueueAccepted
	default:
		return enqueueQueueFull
	}
}

func (g *connectionGeneration) enqueue(conn *websocket.Conn) bool {
	return g.tryEnqueue(conn) == enqueueAccepted
}

// recordQueueFull returns at most one report per interval. The report includes
// the number of similar warnings suppressed since the previous report.
func (g *connectionGeneration) recordQueueFull(now time.Time) (uint64, bool) {
	g.warningMu.Lock()
	defer g.warningMu.Unlock()

	elapsed := now.Sub(g.lastQueueFullWarning)
	if g.lastQueueFullWarning.IsZero() || elapsed < 0 || elapsed >= queueFullWarningInterval {
		suppressed := g.suppressedQueueFullWarnings
		g.lastQueueFullWarning = now
		g.suppressedQueueFullWarnings = 0
		return suppressed, true
	}

	g.suppressedQueueFullWarnings++
	return 0, false
}

func (g *connectionGeneration) next(ctx context.Context) (*websocket.Conn, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-g.done:
			return nil, errGenerationRetired
		case conn := <-g.queue:
			// Linearize taking a queued connection against retirement. If
			// retirement won the race, this connection still belongs to the old
			// generation and must not be used for a new task.
			g.mu.Lock()
			retired := g.retired
			ctxErr := ctx.Err()
			g.mu.Unlock()
			if retired || ctxErr != nil {
				if conn != nil {
					_ = conn.Close()
				}
				if ctxErr != nil {
					return nil, ctxErr
				}
				return nil, errGenerationRetired
			}
			if conn != nil {
				return conn, nil
			}
		}
	}
}

func (g *connectionGeneration) isRetired() bool {
	select {
	case <-g.done:
		return true
	default:
		return false
	}
}

func (g *connectionGeneration) retire() {
	g.mu.Lock()
	if g.retired {
		g.mu.Unlock()
		return
	}
	g.retired = true
	g.cancel()

	var idle []*websocket.Conn
	for {
		select {
		case conn := <-g.queue:
			if conn != nil {
				idle = append(idle, conn)
			}
		default:
			g.mu.Unlock()
			for _, conn := range idle {
				_ = conn.Close()
			}
			return
		}
	}
}

var upgrader = &websocket.Upgrader{
	ReadBufferSize:   0,
	WriteBufferSize:  0,
	HandshakeTimeout: time.Second * 4,
	CheckOrigin: func(r *http.Request) bool {
		return hasSameBrowserOrigin(r)
	},
}

// hasSameBrowserOrigin allows clients that do not send Origin (for example,
// non-browser API clients authenticated by the per-process token). Browsers do
// send Origin for WebSocket and CORS requests, and those requests must match
// both the listener's Host and its HTTP/HTTPS scheme.
func hasSameBrowserOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 || (len(origins) == 1 && origins[0] == "") {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	origin := origins[0]

	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}

	expectedScheme := "http"
	if r.TLS != nil {
		expectedScheme = "https"
	}
	if !strings.EqualFold(u.Scheme, expectedScheme) {
		return false
	}

	requestURL := &url.URL{Scheme: expectedScheme, Host: r.Host}
	return strings.EqualFold(u.Hostname(), requestURL.Hostname()) &&
		effectivePort(u) == effectivePort(requestURL)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// Used by external projects when using xray as a go module
func Reload() {
	addr := platform.NewEnvFlag(platform.BrowserDialerAddress).GetValue(func() string { return "" })
	if err := reload(addr); err != nil {
		errors.LogErrorInner(context.Background(), err, "Browser dialer failed to listen on ", addr)
	}
}

func reload(addr string) error {
	mu.Lock()
	defer mu.Unlock()

	current := activeGeneration.Load()
	if addr == currentAddr {
		if addr == "" && server == nil && current == nil {
			return nil
		}
		if addr != "" && server != nil && current != nil && !current.isRetired() {
			return nil
		}
	}

	oldServer := server
	server = nil
	if oldServer != nil {
		_ = oldServer.Close()
	}

	if addr == "" {
		oldGeneration := activeGeneration.Swap(nil)
		if oldGeneration != nil {
			oldGeneration.retire()
		}
		currentAddr = addr
		return nil
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		oldGeneration := activeGeneration.Swap(nil)
		if oldGeneration != nil {
			oldGeneration.retire()
		}
		currentAddr = addr
		return err
	}

	token := uuid.New()
	csrfToken := token.String()
	page := bytes.ReplaceAll(webpage, []byte("csrfToken"), []byte(csrfToken))
	generation := newConnectionGeneration(connectionQueueSize)
	newServer := &http.Server{
		Addr:    addr,
		Handler: browserHandler(generation, csrfToken, page),
	}

	server = newServer
	currentAddr = addr
	oldGeneration := activeGeneration.Swap(generation)
	if oldGeneration != nil {
		oldGeneration.retire()
	}

	go serve(newServer, listener, generation)
	return nil
}

func browserHandler(generation *connectionGeneration, csrfToken string, page []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasSameBrowserOrigin(r) {
			http.Error(w, "cross-origin browser dialer request denied", http.StatusForbidden)
			return
		}

		switch r.URL.Path {
		case csrfTokenPath:
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(csrfToken))
			return
		case "/websocket":
		default:
			_, _ = w.Write(page)
			return
		}
		if r.URL.Query().Get("token") != csrfToken {
			http.Error(w, "invalid browser dialer token", http.StatusForbidden)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errors.LogErrorInner(r.Context(), err, "Browser dialer http upgrade unexpected error")
			return
		}
		if result := enqueueBrowserConnection(generation, conn); result == enqueueQueueFull {
			if suppressed, report := generation.recordQueueFull(time.Now()); report {
				if suppressed == 0 {
					errors.LogWarning(r.Context(), "Browser dialer connection queue is full; repeated warnings will be aggregated for ", queueFullWarningInterval)
				} else {
					errors.LogWarning(r.Context(), "Browser dialer connection queue is full; suppressed ", suppressed, " similar warnings since the previous report")
				}
			}
		}
	})
}

func enqueueBrowserConnection(generation *connectionGeneration, conn *websocket.Conn) enqueueResult {
	result := generation.tryEnqueue(conn)
	if result == enqueueAccepted {
		return result
	}
	if conn != nil {
		_ = conn.Close()
	}
	return result
}

func serve(httpServer *http.Server, listener net.Listener, generation *connectionGeneration) {
	err := httpServer.Serve(listener)
	if err == nil || stderrors.Is(err, http.ErrServerClosed) {
		return
	}

	errors.LogErrorInner(context.Background(), err, "Browser dialer server stopped unexpectedly")
	mu.Lock()
	if server == httpServer {
		server = nil
		if activeGeneration.CompareAndSwap(generation, nil) {
			generation.retire()
		}
	}
	mu.Unlock()
}

func HasBrowserDialer() bool {
	generation := activeGeneration.Load()
	return generation != nil && !generation.isRetired()
}

type webSocketExtra struct {
	Protocol string `json:"protocol,omitempty"`
}

func DialWS(uri string, ed []byte) (*websocket.Conn, error) {
	return DialWSContext(context.Background(), uri, ed)
}

func DialWSContext(ctx context.Context, uri string, ed []byte) (*websocket.Conn, error) {
	task := task{
		Method:         "WS",
		URL:            uri,
		StreamResponse: true,
	}

	task.Extra = webSocketExtra{
		Protocol: base64.RawURLEncoding.EncodeToString(ed),
	}

	return dialTask(ctx, task)
}

type httpExtra struct {
	Referrer string            `json:"referrer,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Cookies  map[string]string `json:"cookies,omitempty"`
}

func httpExtraFromHeadersAndCookies(headers http.Header, cookies []*http.Cookie) *httpExtra {
	if len(headers) == 0 {
		return nil
	}

	extra := httpExtra{}
	if referrer := headers.Get("Referer"); referrer != "" {
		extra.Referrer = referrer
		headers.Del("Referer")
	}

	if len(headers) > 0 {
		extra.Headers = make(map[string]string)
		for header := range headers {
			extra.Headers[header] = headers.Get(header)
		}
	}

	if len(cookies) > 0 {
		extra.Cookies = make(map[string]string)
		for _, cookie := range cookies {
			extra.Cookies[cookie.Name] = cookie.Value
		}
	}

	return &extra
}

func DialGet(uri string, headers http.Header, cookies []*http.Cookie) (*websocket.Conn, error) {
	return DialGetContext(context.Background(), uri, headers, cookies)
}

func DialGetContext(ctx context.Context, uri string, headers http.Header, cookies []*http.Cookie) (*websocket.Conn, error) {
	task := task{
		Method:         "GET",
		URL:            uri,
		Extra:          httpExtraFromHeadersAndCookies(headers, cookies),
		StreamResponse: true,
	}

	return dialTask(ctx, task)
}

func DialPacket(method string, uri string, headers http.Header, cookies []*http.Cookie, payload []byte) error {
	return DialPacketContext(context.Background(), method, uri, headers, cookies, payload)
}

func DialPacketContext(ctx context.Context, method string, uri string, headers http.Header, cookies []*http.Cookie, payload []byte) error {
	task := task{
		Method:         method,
		URL:            uri,
		Extra:          httpExtraFromHeadersAndCookies(headers, cookies),
		StreamResponse: false,
	}

	conn, err := dialTask(ctx, task)
	if err != nil {
		return err
	}
	stopContext := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopContext()
	defer conn.Close()

	err = conn.WriteMessage(websocket.BinaryMessage, payload)
	if err != nil {
		return contextOrError(ctx, err)
	}

	err = CheckOK(conn)
	if err != nil {
		return contextOrError(ctx, err)
	}
	return ctx.Err()
}

func dialTask(ctx context.Context, task task) (*websocket.Conn, error) {
	data, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		generation := activeGeneration.Load()
		if generation == nil || generation.isRetired() {
			return nil, errors.New("browser dialer is not available")
		}

		conn, err := generation.next(ctx)
		if stderrors.Is(err, errGenerationRetired) {
			continue
		}
		if err != nil {
			return nil, err
		}

		stopContext := context.AfterFunc(ctx, func() { _ = conn.Close() })
		stopGeneration := context.AfterFunc(generation.lifetime, func() { _ = conn.Close() })
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			_ = stopContext()
			_ = stopGeneration()
			_ = conn.Close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			continue
		}

		err = CheckOK(conn)
		_ = stopContext()
		_ = stopGeneration()
		if err != nil {
			if generation.isRetired() && ctx.Err() == nil {
				continue
			}
			return nil, contextOrError(ctx, err)
		}
		if err := ctx.Err(); err != nil {
			_ = conn.Close()
			return nil, err
		}
		if generation.isRetired() {
			_ = conn.Close()
			continue
		}
		return conn, nil
	}
}

func contextOrError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func CheckOK(conn *websocket.Conn) error {
	if _, p, err := conn.ReadMessage(); err != nil {
		conn.Close()
		return err
	} else if s := string(p); s != "ok" {
		conn.Close()
		return errors.New(s)
	}

	return nil
}

func init() {
	platform.RegisterEnvReload(func() error {
		Reload()
		return nil
	})
}
