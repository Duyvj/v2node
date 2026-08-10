package splithttp

import (
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type splitConn struct {
	writer     io.WriteCloser
	reader     io.ReadCloser
	remoteAddr net.Addr
	localAddr  net.Addr
	onClose    func()

	closeOnce sync.Once
	closeErr  error

	deadlineMu         sync.Mutex
	readDeadlineTimer  *time.Timer
	writeDeadlineTimer *time.Timer
	readDeadlineGen    uint64
	writeDeadlineGen   uint64
	closed             atomic.Bool
	readExpired        atomic.Bool
	writeExpired       atomic.Bool
}

func (c *splitConn) Write(b []byte) (int, error) {
	if c.writeExpired.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	n, err := c.writer.Write(b)
	if n == 0 && c.writeExpired.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *splitConn) Read(b []byte) (int, error) {
	if c.readExpired.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	n, err := c.reader.Read(b)
	if n == 0 && c.readExpired.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *splitConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.stopDeadlineTimers()
		if c.onClose != nil {
			c.onClose()
		}

		// Closing the reader first releases a blocked upload/body read. The
		// writer may be waiting on the same HTTP request to be cancelled.
		var readerErr, writerErr error
		if c.reader != nil {
			readerErr = c.reader.Close()
		}
		if c.writer != nil {
			writerErr = c.writer.Close()
		}
		if readerErr != nil {
			c.closeErr = readerErr
		} else {
			c.closeErr = writerErr
		}
	})
	return c.closeErr
}

func (c *splitConn) LocalAddr() net.Addr {
	return c.localAddr
}

func (c *splitConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *splitConn) SetDeadline(t time.Time) error {
	readNative := c.trySetReadDeadline(t)
	writeNative := c.trySetWriteDeadline(t)
	if !readNative {
		c.setReadDeadline(t, true)
	}
	if !writeNative {
		c.setWriteDeadline(t, true)
	}
	return nil
}

func (c *splitConn) SetReadDeadline(t time.Time) error {
	if !c.trySetReadDeadline(t) {
		c.setReadDeadline(t, false)
	}
	return nil
}

func (c *splitConn) SetWriteDeadline(t time.Time) error {
	if !c.trySetWriteDeadline(t) {
		c.setWriteDeadline(t, false)
	}
	return nil
}

func (c *splitConn) trySetReadDeadline(t time.Time) bool {
	if c.closed.Load() {
		return true
	}
	if setter, ok := c.reader.(interface{ SetReadDeadline(time.Time) error }); ok {
		if err := setter.SetReadDeadline(t); err == nil {
			c.clearReadDeadlineTimer()
			return true
		}
	}
	return false
}

func (c *splitConn) trySetWriteDeadline(t time.Time) bool {
	if c.closed.Load() {
		return true
	}
	if setter, ok := c.writer.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := setter.SetWriteDeadline(t); err == nil {
			c.clearWriteDeadlineTimer()
			return true
		}
	}
	return false
}

func (c *splitConn) setReadDeadline(t time.Time, expireBoth bool) {
	// No recoverable directional deadline is available. The timer is terminal
	// and closes both halves, which bounds blocked operations without spawning
	// a goroutine for every Read or deadline update.
	c.deadlineMu.Lock()
	if c.closed.Load() {
		c.deadlineMu.Unlock()
		return
	}
	c.readDeadlineGen++
	gen := c.readDeadlineGen
	if c.readDeadlineTimer != nil {
		c.readDeadlineTimer.Stop()
		c.readDeadlineTimer = nil
	}
	c.readExpired.Store(false)
	if t.IsZero() {
		c.deadlineMu.Unlock()
		return
	}

	d := time.Until(t)
	if d <= 0 {
		c.readExpired.Store(true)
		if expireBoth {
			c.writeExpired.Store(true)
		}
		c.deadlineMu.Unlock()
		_ = c.Close()
		return
	}
	c.readDeadlineTimer = time.AfterFunc(d, func() {
		c.deadlineMu.Lock()
		if gen != c.readDeadlineGen {
			c.deadlineMu.Unlock()
			return
		}
		c.readExpired.Store(true)
		if expireBoth {
			c.writeExpired.Store(true)
		}
		c.deadlineMu.Unlock()
		_ = c.Close()
	})
	c.deadlineMu.Unlock()
}

func (c *splitConn) setWriteDeadline(t time.Time, expireBoth bool) {
	c.deadlineMu.Lock()
	if c.closed.Load() {
		c.deadlineMu.Unlock()
		return
	}
	c.writeDeadlineGen++
	gen := c.writeDeadlineGen
	if c.writeDeadlineTimer != nil {
		c.writeDeadlineTimer.Stop()
		c.writeDeadlineTimer = nil
	}
	c.writeExpired.Store(false)
	if t.IsZero() {
		c.deadlineMu.Unlock()
		return
	}

	d := time.Until(t)
	if d <= 0 {
		c.writeExpired.Store(true)
		if expireBoth {
			c.readExpired.Store(true)
		}
		c.deadlineMu.Unlock()
		_ = c.Close()
		return
	}
	c.writeDeadlineTimer = time.AfterFunc(d, func() {
		c.deadlineMu.Lock()
		if gen != c.writeDeadlineGen {
			c.deadlineMu.Unlock()
			return
		}
		c.writeExpired.Store(true)
		if expireBoth {
			c.readExpired.Store(true)
		}
		c.deadlineMu.Unlock()
		_ = c.Close()
	})
	c.deadlineMu.Unlock()
}

func (c *splitConn) clearReadDeadlineTimer() {
	c.deadlineMu.Lock()
	c.readDeadlineGen++
	if c.readDeadlineTimer != nil {
		c.readDeadlineTimer.Stop()
		c.readDeadlineTimer = nil
	}
	c.readExpired.Store(false)
	c.deadlineMu.Unlock()
}

func (c *splitConn) clearWriteDeadlineTimer() {
	c.deadlineMu.Lock()
	c.writeDeadlineGen++
	if c.writeDeadlineTimer != nil {
		c.writeDeadlineTimer.Stop()
		c.writeDeadlineTimer = nil
	}
	c.writeExpired.Store(false)
	c.deadlineMu.Unlock()
}

func (c *splitConn) stopDeadlineTimers() {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readDeadlineGen++
	c.writeDeadlineGen++
	if c.readDeadlineTimer != nil {
		c.readDeadlineTimer.Stop()
		c.readDeadlineTimer = nil
	}
	if c.writeDeadlineTimer != nil {
		c.writeDeadlineTimer.Stop()
		c.writeDeadlineTimer = nil
	}
}
