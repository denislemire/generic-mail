package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testKeyPair(t *testing.T, serial int64) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
func servedSerial(t *testing.T, r *certificateReloader) int64 {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	serverConn.SetDeadline(time.Now().Add(5 * time.Second))
	clientConn.SetDeadline(time.Now().Add(5 * time.Second))
	server := tls.Server(serverConn, &tls.Config{GetCertificate: r.getCertificate, MinVersion: tls.VersionTLS12})
	client := tls.Client(clientConn, &tls.Config{InsecureSkipVerify: true}) // Test-only certificates.
	done := make(chan error, 1)
	go func() { done <- server.Handshake() }()
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return client.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}
func TestCertificateRotation(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	cert1, key1 := testKeyPair(t, 1)
	cert2, key2 := testKeyPair(t, 2)
	write := func(path string, body []byte) {
		t.Helper()
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(certPath, cert1)
	write(keyPath, key1)
	r, err := newCertificateReloader(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if servedSerial(t, r) != 1 {
		t.Fatal("wrong initial certificate")
	}
	write(certPath, cert2)
	if err = r.reload(); err == nil {
		t.Fatal("accepted mismatched certificate/key")
	}
	if servedSerial(t, r) != 1 {
		t.Fatal("partial update replaced working certificate")
	}
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				r.getCertificate(nil)
			}
		}()
	}
	write(keyPath, key2)
	if err = r.reload(); err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	if servedSerial(t, r) != 2 {
		t.Fatal("renewed certificate was not served")
	}
}
func TestInvalidInitialCertificate(t *testing.T) {
	if _, err := newCertificateReloader(filepath.Join(t.TempDir(), "missing"), "missing"); err == nil {
		t.Fatal("startup accepted missing credentials")
	}
}
