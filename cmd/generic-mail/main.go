package main

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"
	"github.com/denislemire/generic-mail/internal/imapd"
	"github.com/denislemire/generic-mail/internal/mailbox"
	"github.com/denislemire/generic-mail/internal/oauth"
	"github.com/denislemire/generic-mail/internal/sesmail"
	"github.com/denislemire/generic-mail/internal/smtpd"

	"github.com/emersion/go-imap/v2/imapserver"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	username := os.Getenv("MAIL_USERNAME")
	password := os.Getenv("MAIL_PASSWORD")
	bucket := os.Getenv("S3_BUCKET")
	if username == "" || password == "" || bucket == "" {
		log.Fatal("MAIL_USERNAME, MAIL_PASSWORD, and S3_BUCKET are required")
	}
	prefix := env("S3_INBOUND_PREFIX", "inbound/")
	s3Region := env("S3_REGION", "us-east-1")
	sesRegion := env("SES_REGION", "ca-central-1")
	hostname := os.Getenv("MAIL_HOSTNAME")
	if hostname == "" {
		log.Fatal("MAIL_HOSTNAME is required")
	}
	dataDir := env("MAIL_DATA", "/var/lib/generic-mail")

	store, err := blob.NewS3(ctx, bucket, s3Region)
	if err != nil {
		log.Fatalf("s3: %v", err)
	}
	engine, err := mailbox.New(ctx, store, dataDir)
	if err != nil {
		log.Fatalf("mailbox: %v", err)
	}
	sesClient, err := sesmail.New(ctx, sesRegion)
	if err != nil {
		log.Fatalf("ses: %v", err)
	}
	checker, err := oauth.New(oauth.Config{
		Issuer:   os.Getenv("OAUTH_ISSUER"),
		JWKSURL:  os.Getenv("OAUTH_JWKS_URL"),
		Audience: os.Getenv("OAUTH_AUDIENCE"),
		Allowed:  strings.Split(os.Getenv("OAUTH_ALLOWED"), ","),
	})
	if err != nil {
		log.Fatalf("oauth: %v", err)
	}

	cert, err := tls.LoadX509KeyPair(env("TLS_CERT", "/tls/tls.crt"), env("TLS_KEY", "/tls/tls.key"))
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}

	imapSrv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return (&imapd.Server{Engine: engine, Username: username, Password: password, OAuth: checker}).NewSession(), &imapserver.GreetingData{}, nil
		},
		TLSConfig:    tlsConfig,
		InsecureAuth: false,
	})
	submission := &smtpd.Server{
		Hostname: hostname,
		Username: username,
		Password: password,
		Sender:   sesClient,
		OAuth:    checker,
	}

	errCh := make(chan error, 4)
	go func() { errCh <- serveIMAP(imapSrv, env("LISTEN_IMAPS", ":993"), tlsConfig) }()
	go func() { errCh <- serveSMTP(submission, env("LISTEN_SUBMISSION", ":587"), tlsConfig, false) }()
	go func() { errCh <- serveSMTP(submission, env("LISTEN_SMTPS", ":465"), tlsConfig, true) }()
	go func() { errCh <- serveHealth(engine, env("LISTEN_HEALTH", ":8080")) }()

	interval := 30 * time.Second
	if v := os.Getenv("SYNC_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			interval = d
		}
	}
	go func() {
		if err := engine.SyncInbound(ctx, prefix); err != nil {
			log.Printf("initial sync: %v", err)
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := engine.SyncInbound(ctx, prefix); err != nil {
					log.Printf("sync: %v", err)
				}
			}
		}
	}()

	log.Printf("generic-mail hostname=%s bucket=%s", hostname, bucket)
	select {
	case <-ctx.Done():
		log.Printf("shutting down")
	case err := <-errCh:
		log.Fatalf("listener: %v", err)
	}
}

func serveIMAP(srv *imapserver.Server, addr string, tlsConfig *tls.Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return srv.Serve(tls.NewListener(ln, tlsConfig))
}

func serveSMTP(srv *smtpd.Server, addr string, tlsConfig *tls.Config, implicit bool) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if implicit {
		ln = tls.NewListener(ln, tlsConfig)
	}
	return srv.Serve(ln, tlsConfig)
}

func serveHealth(engine *mailbox.Engine, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !engine.Ready() {
			http.Error(w, "sync pending", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return http.ListenAndServe(addr, mux)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
