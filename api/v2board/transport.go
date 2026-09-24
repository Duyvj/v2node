package panel

import (
	"errors"
	"io"
	"net/http"
)

const maxPanelBody = 16 << 20

var errPanelBodyTooLarge = errors.New("panel response exceeds 16 MiB")

type boundedTransport struct{ base *http.Transport }

func (t *boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r, err := t.base.RoundTrip(req)
	if r != nil && r.Body != nil {
		r.Body = &boundedBody{ReadCloser: r.Body, remaining: maxPanelBody}
	}
	return r, err
}
func (t *boundedTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, errPanelBodyTooLarge
	}
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return n, errPanelBodyTooLarge
	}
	return n, err
}
