package main

import (
	"context"
	"crypto/tls"
	"log"
	"sync"
	"time"
)

// certificateReloader serves the last complete, valid key pair while the mounted
// Secret rotates. A partial or invalid update never replaces the active pair.
type certificateReloader struct {
	certPath, keyPath string
	mu                sync.RWMutex
	cert              *tls.Certificate
}

func newCertificateReloader(certPath, keyPath string) (*certificateReloader, error) {
	r := &certificateReloader{certPath: certPath, keyPath: keyPath}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}
func (r *certificateReloader) reload() error {
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cert = &cert
	r.mu.Unlock()
	return nil
}
func (r *certificateReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}
func (r *certificateReloader) watch(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.reload(); err != nil {
				log.Printf("TLS reload: retaining previous certificate: %v", err)
			}
		}
	}
}
