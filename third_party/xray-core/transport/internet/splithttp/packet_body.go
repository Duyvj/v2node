package splithttp

import (
	"io"
	"sync"

	"github.com/xtls/xray-core/common/buf"
)

// packetBody gives net/http the required concurrent Read/Close safety while
// retaining ownership of unread MultiBuffers on cancellation.
type packetBody struct {
	mu        sync.Mutex
	container *buf.MultiBufferContainer
}

func newPacketBody(payload buf.MultiBuffer) *packetBody {
	return &packetBody{container: &buf.MultiBufferContainer{MultiBuffer: payload}}
}

func (b *packetBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.container == nil {
		return 0, io.EOF
	}
	return b.container.Read(p)
}

func (b *packetBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.container == nil {
		return nil
	}
	err := b.container.Close()
	b.container = nil
	return err
}
