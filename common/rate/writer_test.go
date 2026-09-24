package rate

import (
	"context"
	"errors"
	"github.com/xtls/xray-core/common/buf"
	"testing"
)

type sink struct{ bytes int32 }

func (s *sink) WriteMultiBuffer(mb buf.MultiBuffer) error {
	s.bytes += mb.Len()
	buf.ReleaseMulti(mb)
	return nil
}
func TestCancelledWriterDoesNotForward(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := &sink{}
	w := NewRateLimitWriterContext(ctx, out, NewDynamicBucket(1))
	if err := w.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("hello"))}); !errors.Is(err, context.Canceled) || out.bytes != 0 {
		t.Fatal("cancel ignored")
	}
}
func TestZeroRateMeansUnlimited(t *testing.T) {
	b := NewDynamicBucket(100)
	b.Update(0)
	if b.Get() != nil {
		t.Fatal("zero rate retained limiter")
	}
}
