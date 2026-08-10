package anytls

import (
	"context"
	"sync"

	"github.com/xtls/xray-core/common"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport"
)

type stream struct {
	sid  uint32
	link *transport.Link

	done     chan struct{}
	doneOnce sync.Once
	errMu    sync.Mutex
	err      error

	dieHookMu        sync.Mutex
	dieHook          func()
	dieHookInstalled bool
	dead             bool

	dispatchCtx  context.Context
	isUDP        bool
	udpTarget    *xnet.Destination
	udpIsConnect bool
}

func newStream(sid uint32, link *transport.Link) *stream {
	return &stream{
		sid:  sid,
		link: link,
		done: make(chan struct{}),
	}
}

func (st *stream) close(err error) {
	if st.done == nil {
		if st.link != nil {
			common.Close(st.link.Reader)
			common.Close(st.link.Writer)
		}
		return
	}
	st.doneOnce.Do(func() {
		st.errMu.Lock()
		st.err = err
		st.errMu.Unlock()
		if st.link != nil {
			common.Close(st.link.Reader)
			common.Close(st.link.Writer)
		}
		close(st.done)
		st.fireDieHook()
	})
}

// installDieHook installs the stream completion callback, or runs it
// immediately when the stream has already completed. A peer may send FIN as
// soon as openStream publishes the stream to the session, before Process gets
// a chance to install this hook.
func (st *stream) installDieHook(hook func()) {
	if hook == nil {
		return
	}

	st.dieHookMu.Lock()
	if st.dieHookInstalled {
		st.dieHookMu.Unlock()
		return
	}
	st.dieHookInstalled = true
	if !st.dead {
		st.dieHook = hook
		st.dieHookMu.Unlock()
		return
	}
	st.dieHookMu.Unlock()

	hook()
}

func (st *stream) fireDieHook() {
	st.dieHookMu.Lock()
	st.dead = true
	hook := st.dieHook
	st.dieHook = nil
	st.dieHookMu.Unlock()

	if hook != nil {
		hook()
	}
}

func (st *stream) result() error {
	st.errMu.Lock()
	defer st.errMu.Unlock()
	return st.err
}
