package tls

import (
	"context"
	"crypto/tls"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/ocsp"
)

type certificateSnapshot struct{ certs []*tls.Certificate }

// Static file loading avoids the legacy uncancellable hot-reload worker.
// OCSP is refreshed independently and stops with the owning core context.
func managedCertificateGetter(ctx context.Context, certs []*tls.Certificate, entries []*Certificate, reject bool) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	snapshot := &atomic.Pointer[certificateSnapshot]{}
	snapshot.Store(&certificateSnapshot{certs: certs})
	index := 0
	for _, entry := range entries {
		if entry.Usage != Certificate_ENCIPHERMENT {
			continue
		}
		if _, err := tls.X509KeyPair(entry.Certificate, entry.Key); err != nil {
			continue
		}
		current := index
		index++
		if !entry.OneTimeLoading || entry.OcspStapling == 0 || current >= len(certs) || len(certs[current].Leaf.OCSPServer) == 0 {
			continue
		}
		go refreshOCSP(ctx, snapshot, current, time.Duration(entry.OcspStapling)*time.Second)
	}
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return selectCertificate(snapshot.Load().certs, reject, hello)
	}
}

func refreshOCSP(ctx context.Context, snapshot *atomic.Pointer[certificateSnapshot], index int, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		original := snapshot.Load().certs[index]
		if response, err := ocsp.GetOCSPForCertContext(ctx, original.Certificate); err == nil {
			for {
				previous := snapshot.Load()
				cert := *previous.certs[index]
				cert.OCSPStaple = response
				updated := append([]*tls.Certificate(nil), previous.certs...)
				updated[index] = &cert
				if snapshot.CompareAndSwap(previous, &certificateSnapshot{certs: updated}) {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
