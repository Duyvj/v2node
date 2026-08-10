package splithttp

import (
	"bufio"
	"errors"
	"net"
)

const maxH1ResponseHeaderBytes = 1 << 20

var errH1ResponseHeaderTooLarge = errors.New("HTTP/1.1 upload response header exceeded 1 MiB")

type responseHeaderLimitReader struct {
	net.Conn
	remaining int64
	limited   bool
}

func (r *responseHeaderLimitReader) Read(p []byte) (int, error) {
	if !r.limited || len(p) == 0 {
		return r.Conn.Read(p)
	}
	if r.remaining <= 0 {
		return 0, errH1ResponseHeaderTooLarge
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.Conn.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func (r *responseHeaderLimitReader) start() {
	r.remaining = maxH1ResponseHeaderBytes
	r.limited = true
}

func (r *responseHeaderLimitReader) finish() {
	r.limited = false
}

type H1Conn struct {
	RespBufReader  *bufio.Reader
	responseReader *responseHeaderLimitReader
	net.Conn
}

func NewH1Conn(conn net.Conn) *H1Conn {
	responseReader := &responseHeaderLimitReader{Conn: conn}
	return &H1Conn{
		RespBufReader:  bufio.NewReader(responseReader),
		responseReader: responseReader,
		Conn:           conn,
	}
}

func (c *H1Conn) startResponseHeader() {
	c.responseReader.start()
}

func (c *H1Conn) finishResponseHeader() {
	c.responseReader.finish()
}
