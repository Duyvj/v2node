package rate

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/juju/ratelimit"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
)

type Writer struct {
	writer  buf.Writer
	limiter *DynamicBucket
	ctx     context.Context
}

type DynamicBucket struct {
	v atomic.Value // *ratelimit.Bucket
}

func NewDynamicBucket(rate int64) *DynamicBucket {
	d := &DynamicBucket{}
	d.Update(rate)
	return d
}

func (d *DynamicBucket) Get() *ratelimit.Bucket {
	return d.v.Load().(*ratelimit.Bucket)
}

func (d *DynamicBucket) Update(rate int64) {
	if rate <= 0 {
		d.v.Store((*ratelimit.Bucket)(nil))
		return
	}
	newB := ratelimit.NewBucketWithQuantum(time.Second, rate, rate)
	d.v.Store(newB)
}

func NewRateLimitWriter(writer buf.Writer, limiter *DynamicBucket) buf.Writer {
	return NewRateLimitWriterContext(context.Background(), writer, limiter)
}

func NewRateLimitWriterContext(ctx context.Context, writer buf.Writer, limiter *DynamicBucket) buf.Writer {
	return &Writer{
		writer:  writer,
		limiter: limiter,
		ctx:     ctx,
	}
}

func (w *Writer) Close() error {
	return common.Close(w.writer)
}

func (w *Writer) WriteMultiBuffer(mb buf.MultiBuffer) error {
	if err := w.ctx.Err(); err != nil {
		buf.ReleaseMulti(mb)
		return err
	}
	limiter := w.limiter.Get()
	if limiter != nil {
		if delay := limiter.Take(int64(mb.Len())); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-w.ctx.Done():
				timer.Stop()
				buf.ReleaseMulti(mb)
				return w.ctx.Err()
			case <-timer.C:
			}
		}
	}
	return w.writer.WriteMultiBuffer(mb)
}
