package core

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/task"
)

type certificateFiles struct {
	certPath, keyPath string
	fingerprint       [32]byte
}

type certificateMonitor struct {
	mu     sync.Mutex
	files  map[string]certificateFiles
	worker *task.Task
}

func certificateFingerprint(certPath, keyPath string) ([32]byte, error) {
	cert, err := os.ReadFile(certPath)
	if err != nil {
		return [32]byte{}, err
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return [32]byte{}, err
	}
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		return [32]byte{}, fmt.Errorf("invalid TLS key pair: %w", err)
	}
	hash := sha256.New()
	hash.Write(cert)
	hash.Write(key)
	var fingerprint [32]byte
	copy(fingerprint[:], hash.Sum(nil))
	return fingerprint, nil
}

func (v *V2Core) watchCertificate(tag string, info *panel.NodeInfo) error {
	if info.Security != panel.Tls || info.Common.CertInfo == nil {
		return nil
	}
	cert := info.Common.CertInfo
	if cert.CertMode == "" || cert.CertMode == "none" {
		return nil
	}
	fingerprint, err := certificateFingerprint(cert.CertFile, cert.KeyFile)
	if err != nil {
		return err
	}
	m := &v.certificates
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.files == nil {
		m.files = make(map[string]certificateFiles)
	}
	m.files[tag] = certificateFiles{cert.CertFile, cert.KeyFile, fingerprint}
	if m.worker == nil {
		m.worker = &task.Task{Name: "certificateRefresh", Interval: time.Hour, Execute: v.checkCertificateUpdates}
		if err := m.worker.Start(false); err != nil {
			m.worker = nil
			delete(m.files, tag)
			return err
		}
	}
	return nil
}

// Match the previous hourly refresh interval. Invalid or half-written files
// keep the currently loaded key pair, and are retried on the next interval.
func (v *V2Core) checkCertificateUpdates(ctx context.Context) error {
	m := &v.certificates
	m.mu.Lock()
	defer m.mu.Unlock()
	var failures []error
	for tag, files := range m.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		fingerprint, err := certificateFingerprint(files.certPath, files.keyPath)
		if err != nil {
			failures = append(failures, fmt.Errorf("refresh certificate %s: %w", tag, err))
			continue
		}
		if fingerprint == files.fingerprint {
			continue
		}
		if v.ReloadCh != nil {
			select {
			case v.ReloadCh <- struct{}{}:
			default:
			}
			files.fingerprint = fingerprint
			m.files[tag] = files
		}
	}
	return errors.Join(failures...)
}

func (v *V2Core) closeCertificateMonitor() error {
	m := &v.certificates
	m.mu.Lock()
	worker := m.worker
	m.mu.Unlock()
	if worker != nil {
		if err := worker.Close(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.worker = nil
	m.files = nil
	m.mu.Unlock()
	return nil
}
