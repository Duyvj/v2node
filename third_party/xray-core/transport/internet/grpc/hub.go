package grpc

import (
	"context"
	stderrors "errors"
	stdnet "net"
	"sync"
	"time"

	goreality "github.com/xtls/reality"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/grpc/encoding"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/tls"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

type Listener struct {
	encoding.UnimplementedGRPCServiceServer
	ctx                  context.Context
	handler              internet.ConnHandler
	local                net.Addr
	config               *Config
	trustedXForwardedFor []string

	s *grpc.Server

	mu             sync.Mutex
	streamListener net.Listener
	active         map[net.Conn]struct{}
	closed         chan struct{}
	isClosed       bool
	closeOnce      sync.Once
	closeErr       error
	stopContext    func() bool
}

func (l *Listener) Tun(server encoding.GRPCService_TunServer) error {
	tunCtx, cancel := context.WithCancel(l.ctx)
	stopStreamContext := context.AfterFunc(server.Context(), cancel)
	defer stopStreamContext()
	conn := encoding.NewHunkConn(server, cancel, l.trustedXForwardedFor)
	return l.serveTunnel(tunCtx, conn)
}

func (l *Listener) TunMulti(server encoding.GRPCService_TunMultiServer) error {
	tunCtx, cancel := context.WithCancel(l.ctx)
	stopStreamContext := context.AfterFunc(server.Context(), cancel)
	defer stopStreamContext()
	conn := encoding.NewMultiHunkConn(server, cancel, l.trustedXForwardedFor)
	return l.serveTunnel(tunCtx, conn)
}

func (l *Listener) serveTunnel(ctx context.Context, conn net.Conn) error {
	if !l.track(conn) {
		_ = conn.Close()
		return nil
	}
	defer func() {
		l.untrack(conn)
		_ = conn.Close()
	}()

	l.handler(conn)
	select {
	case <-ctx.Done():
	case <-l.closed:
	}
	return nil
}

func (l *Listener) track(conn net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isClosed {
		return false
	}
	l.active[conn] = struct{}{}
	return true
}

func (l *Listener) untrack(conn net.Conn) {
	l.mu.Lock()
	delete(l.active, conn)
	l.mu.Unlock()
}

func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.isClosed = true
		close(l.closed)
		stopContext := l.stopContext
		l.stopContext = nil
		streamListener := l.streamListener
		active := make([]net.Conn, 0, len(l.active))
		for conn := range l.active {
			active = append(active, conn)
		}
		l.active = nil
		l.mu.Unlock()
		if stopContext != nil {
			stopContext()
		}

		if streamListener != nil {
			if err := streamListener.Close(); err != nil && !stderrors.Is(err, stdnet.ErrClosed) {
				l.closeErr = err
			}
		}
		for _, conn := range active {
			_ = conn.Close()
		}
		if l.s != nil {
			l.s.Stop()
		}
	})
	return l.closeErr
}

func (l *Listener) installStopContext(ctx context.Context) {
	stopContext := context.AfterFunc(ctx, func() {
		_ = l.Close()
	})
	l.mu.Lock()
	if l.isClosed {
		l.mu.Unlock()
		stopContext()
		return
	}
	l.stopContext = stopContext
	l.mu.Unlock()
}

func (l *Listener) Addr() net.Addr {
	return l.local
}

func Listen(ctx context.Context, address net.Address, port net.Port, settings *internet.MemoryStreamConfig, handler internet.ConnHandler) (internet.Listener, error) {
	grpcSettings := settings.ProtocolSettings.(*Config)
	var listener *Listener
	if port == net.Port(0) { // unix
		listener = &Listener{
			handler: handler,
			local: &net.UnixAddr{
				Name: address.Domain(),
				Net:  "unix",
			},
			config: grpcSettings,
		}
	} else { // tcp
		listener = &Listener{
			handler: handler,
			local: &net.TCPAddr{
				IP:   address.IP(),
				Port: int(port),
			},
			config: grpcSettings,
		}
	}

	listener.ctx = ctx
	listener.active = make(map[net.Conn]struct{})
	listener.closed = make(chan struct{})
	if settings.SocketSettings != nil {
		listener.trustedXForwardedFor = settings.SocketSettings.TrustedXForwardedFor
	}

	config := tls.ConfigFromStreamSettings(settings)

	var options []grpc.ServerOption
	var s *grpc.Server
	if config != nil {
		// gRPC server may silently ignore TLS errors
		options = append(options, grpc.Creds(credentials.NewTLS(config.GetTLSConfig(tls.WithNextProto("h2")))))
	}
	if grpcSettings.IdleTimeout > 0 || grpcSettings.HealthCheckTimeout > 0 {
		options = append(options, grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    time.Second * time.Duration(grpcSettings.IdleTimeout),
			Timeout: time.Second * time.Duration(grpcSettings.HealthCheckTimeout),
		}))
	}

	s = grpc.NewServer(options...)
	listener.s = s
	encoding.RegisterGRPCServiceServerX(s, listener, grpcSettings.getServiceName(), grpcSettings.getTunStreamName(), grpcSettings.getTunMultiStreamName())
	listener.installStopContext(ctx)

	if settings.SocketSettings != nil && settings.SocketSettings.AcceptProxyProtocol {
		errors.LogWarning(ctx, "accepting PROXY protocol")
	}

	go func() {
		var streamListener net.Listener
		var err error
		if port == net.Port(0) { // unix
			streamListener, err = internet.ListenSystem(ctx, &net.UnixAddr{
				Name: address.Domain(),
				Net:  "unix",
			}, settings.SocketSettings)
			if err != nil {
				errors.LogErrorInner(ctx, err, "failed to listen on ", address)
				return
			}
		} else { // tcp
			streamListener, err = internet.ListenSystem(ctx, &net.TCPAddr{
				IP:   address.IP(),
				Port: int(port),
			}, settings.SocketSettings)
			if err != nil {
				errors.LogErrorInner(ctx, err, "failed to listen on ", address, ":", port)
				return
			}
		}

		if settings.TcpmaskManager != nil {
			streamListener, _ = settings.TcpmaskManager.WrapListener(streamListener)
		}

		if config := reality.ConfigFromStreamSettings(settings); config != nil {
			streamListener = goreality.NewListener(streamListener, config.GetREALITYConfig())
		}

		listener.mu.Lock()
		if listener.isClosed {
			listener.mu.Unlock()
			_ = streamListener.Close()
			return
		}
		listener.streamListener = streamListener
		listener.mu.Unlock()

		errors.LogDebug(ctx, "gRPC listen for service name `"+grpcSettings.getServiceName()+"` tun `"+grpcSettings.getTunStreamName()+"` multi tun `"+grpcSettings.getTunMultiStreamName()+"`")
		if err = s.Serve(streamListener); err != nil {
			errors.LogInfoInner(ctx, err, "Listener for gRPC ended")
		}
	}()

	return listener, nil
}

func init() {
	common.Must(internet.RegisterTransportListener(protocolName, Listen))
}
