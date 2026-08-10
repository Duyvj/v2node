package splithttp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go/http3"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/signal/done"
)

// interface to abstract between use of browser dialer, vs net/http
type DialerClient interface {
	IsClosed() bool

	// ctx, url, sessionId, body, uploadOnly
	OpenStream(context.Context, string, string, io.Reader, bool) (io.ReadCloser, net.Addr, net.Addr, error)

	// ctx, url, sessionId, seqStr, body, contentLength
	PostPacket(context.Context, string, string, string, buf.MultiBuffer) error
}

// implements splithttp.DialerClient in terms of direct network connections
type DefaultDialerClient struct {
	transportConfig *Config
	client          *http.Client
	closed          atomic.Bool
	httpVersion     string

	uploadMu       sync.Mutex
	uploadIdle     []*H1Conn
	uploadAll      map[*H1Conn]struct{}
	dialUploadConn func(ctxInner context.Context) (net.Conn, error)
}

const (
	responseHeaderTimeout = 30 * time.Second
	maxIdleH1UploadConns  = 64
)

func (c *DefaultDialerClient) IsClosed() bool {
	return c.closed.Load()
}

func (c *DefaultDialerClient) OpenStream(ctx context.Context, url string, sessionId string, body io.Reader, uploadOnly bool) (wrc io.ReadCloser, remoteAddr, localAddr net.Addr, err error) {
	// this is done when the TCP/UDP connection to the server was established,
	// and we can unblock the Dial function and print correct net addresses in
	// logs
	gotConn := done.New()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(connInfo httptrace.GotConnInfo) {
			remoteAddr = connInfo.Conn.RemoteAddr()
			localAddr = connInfo.Conn.LocalAddr()
			gotConn.Close()
		},
	})

	method := "GET" // stream-down
	if body != nil {
		method = c.transportConfig.GetNormalizedUplinkHTTPMethod() // stream-up/one
	}
	reqCtx, cancelRequest := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, method, url, body)
	if err != nil {
		cancelRequest()
		errors.LogInfoInner(ctx, err, "failed to create HTTP request for "+url)
		return nil, nil, nil, err
	}
	c.transportConfig.FillStreamRequest(req, sessionId, "")

	wrc = &WaitReadCloser{Wait: make(chan struct{})}
	go func() {
		headerTimer := time.AfterFunc(responseHeaderTimeout, cancelRequest)
		resp, err := c.client.Do(req)
		headerTimer.Stop()
		if err != nil {
			if !uploadOnly { // stream-down is enough
				c.closed.Store(true)
				errors.LogInfoInner(ctx, err, "failed to "+method+" "+url)
			}
			gotConn.Close()
			common.Close(body)
			wrc.Close()
			cancelRequest()
			return
		}
		if resp.StatusCode != 200 && !uploadOnly {
			errors.LogInfo(ctx, "unexpected status ", resp.StatusCode)
		}
		if resp.StatusCode != 200 || uploadOnly { // stream-up
			_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
			resp.Body.Close() // if it is called immediately, the upload will be interrupted also
			common.Close(body)
			wrc.Close()
			cancelRequest()
			return
		}
		wrc.(*WaitReadCloser).Set(&cancelOnCloseReadCloser{ReadCloser: resp.Body, cancel: cancelRequest})
	}()

	<-gotConn.Wait()
	return
}

func (c *DefaultDialerClient) PostPacket(ctx context.Context, url string, sessionId string, seqStr string, payload buf.MultiBuffer) error {
	method := c.transportConfig.GetNormalizedUplinkHTTPMethod()
	reqCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	req, err := http.NewRequestWithContext(reqCtx, method, url, nil)
	if err != nil {
		return err
	}
	if err := c.transportConfig.FillPacketRequest(req, sessionId, seqStr, payload); err != nil {
		return err
	}
	defer common.Close(req.Body)

	if c.httpVersion != "1.1" {
		headerTimer := time.AfterFunc(responseHeaderTimeout, cancelRequest)
		resp, err := c.client.Do(req)
		headerTimer.Stop()
		if err != nil {
			c.closed.Store(true)
			return err
		}

		_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return errors.New("bad status code:", resp.Status)
		}
	} else {
		// stringify the entire HTTP/1.1 request so it can be
		// safely retried. if instead req.Write is called multiple
		// times, the body is already drained after the first
		// request
		requestBuff := new(bytes.Buffer)
		requestBuff.Grow(512 + int(req.ContentLength))
		common.Must(req.Write(requestBuff))

		for {
			retry, err := c.doH1Upload(ctx, req, requestBuff.Bytes())
			if err == nil || !retry || ctx.Err() != nil {
				return err
			}
		}
	}

	return nil
}

func (c *DefaultDialerClient) Close() error {
	c.closed.Store(true)
	c.closeH1UploadConns()
	transport := c.client.Transport
	if h3Transport, ok := transport.(*http3.Transport); ok {
		return h3Transport.Close()
	}
	if idleCloser, ok := transport.(interface{ CloseIdleConnections() }); ok {
		idleCloser.CloseIdleConnections()
	}
	return nil
}

func (c *DefaultDialerClient) getH1UploadConn(ctx context.Context) (*H1Conn, bool, error) {
	c.uploadMu.Lock()
	if c.closed.Load() {
		c.uploadMu.Unlock()
		return nil, false, io.ErrClosedPipe
	}
	if n := len(c.uploadIdle); n > 0 {
		conn := c.uploadIdle[n-1]
		c.uploadIdle[n-1] = nil
		c.uploadIdle = c.uploadIdle[:n-1]
		c.uploadMu.Unlock()
		return conn, true, nil
	}
	c.uploadMu.Unlock()

	raw, err := c.dialUploadConn(ctx)
	if err != nil {
		return nil, false, err
	}
	conn := NewH1Conn(raw)
	c.uploadMu.Lock()
	if c.closed.Load() {
		c.uploadMu.Unlock()
		_ = conn.Close()
		return nil, false, io.ErrClosedPipe
	}
	if c.uploadAll == nil {
		c.uploadAll = make(map[*H1Conn]struct{})
	}
	c.uploadAll[conn] = struct{}{}
	c.uploadMu.Unlock()
	return conn, false, nil
}

// doH1Upload retries only a write that failed on a pooled connection. Once a
// request write succeeds, retrying could duplicate a packet at the server.
func (c *DefaultDialerClient) doH1Upload(ctx context.Context, req *http.Request, request []byte) (retry bool, err error) {
	conn, reused, err := c.getH1UploadConn(ctx)
	if err != nil {
		return false, err
	}
	keep := false
	defer func() {
		if keep {
			c.putH1UploadConn(conn)
		} else {
			c.discardH1UploadConn(conn)
		}
	}()

	stopCancel := context.AfterFunc(ctx, func() { c.discardH1UploadConn(conn) })
	defer stopCancel()
	_ = conn.SetWriteDeadline(time.Now().Add(responseHeaderTimeout))
	if _, err := conn.Write(request); err != nil {
		return reused, err
	}
	_ = conn.SetWriteDeadline(time.Time{})
	_ = conn.SetReadDeadline(time.Now().Add(responseHeaderTimeout))
	conn.startResponseHeader()
	resp, err := http.ReadResponse(conn.RespBufReader, req)
	conn.finishResponseHeader()
	if err != nil {
		c.closed.Store(true)
		return false, fmt.Errorf("error while reading response: %w", err)
	}
	_, copyErr := io.CopyN(io.Discard, resp.Body, 64<<10)
	closeErr := resp.Body.Close()
	_ = conn.SetReadDeadline(time.Time{})
	bodyTooLarge := copyErr == nil
	if copyErr != io.EOF {
		if !bodyTooLarge {
			return false, copyErr
		}
	}
	if closeErr != nil && !bodyTooLarge {
		return false, closeErr
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("got non-200 error response code: %d", resp.StatusCode)
	}
	// A successful oversized body is not a protocol failure, but the raw
	// connection cannot be reused because unread bytes remain on it.
	keep = !resp.Close && !bodyTooLarge
	return false, nil
}

func (c *DefaultDialerClient) putH1UploadConn(conn *H1Conn) {
	if conn == nil {
		return
	}
	c.uploadMu.Lock()
	_, tracked := c.uploadAll[conn]
	if c.closed.Load() || !tracked || len(c.uploadIdle) >= maxIdleH1UploadConns {
		delete(c.uploadAll, conn)
		c.uploadMu.Unlock()
		_ = conn.Close()
		return
	}
	c.uploadIdle = append(c.uploadIdle, conn)
	c.uploadMu.Unlock()
}

func (c *DefaultDialerClient) discardH1UploadConn(conn *H1Conn) {
	if conn == nil {
		return
	}
	c.uploadMu.Lock()
	delete(c.uploadAll, conn)
	for i, idle := range c.uploadIdle {
		if idle == conn {
			copy(c.uploadIdle[i:], c.uploadIdle[i+1:])
			c.uploadIdle[len(c.uploadIdle)-1] = nil
			c.uploadIdle = c.uploadIdle[:len(c.uploadIdle)-1]
			break
		}
	}
	c.uploadMu.Unlock()
	_ = conn.Close()
}

func (c *DefaultDialerClient) closeH1UploadConns() {
	c.uploadMu.Lock()
	connections := make([]*H1Conn, 0, len(c.uploadAll))
	for conn := range c.uploadAll {
		connections = append(connections, conn)
	}
	c.uploadAll = nil
	for i := range c.uploadIdle {
		c.uploadIdle[i] = nil
	}
	c.uploadIdle = nil
	c.uploadMu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

type cancelOnCloseReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnCloseReadCloser) Close() error {
	c.once.Do(c.cancel)
	return c.ReadCloser.Close()
}

type WaitReadCloser struct {
	Wait chan struct{}

	mu        sync.Mutex
	readyOnce sync.Once
	closer    io.ReadCloser
	closed    bool
}

func (w *WaitReadCloser) Set(rc io.ReadCloser) {
	w.mu.Lock()
	if w.closed || w.closer != nil {
		w.mu.Unlock()
		_ = rc.Close()
		return
	}
	w.closer = rc
	w.readyOnce.Do(func() { close(w.Wait) })
	w.mu.Unlock()
}

func (w *WaitReadCloser) Read(b []byte) (int, error) {
	<-w.Wait
	w.mu.Lock()
	closer := w.closer
	closed := w.closed
	w.mu.Unlock()
	if closed || closer == nil {
		return 0, io.ErrClosedPipe
	}
	return closer.Read(b)
}

func (w *WaitReadCloser) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	closer := w.closer
	w.readyOnce.Do(func() { close(w.Wait) })
	w.mu.Unlock()
	if closer != nil {
		return closer.Close()
	}
	return nil
}
