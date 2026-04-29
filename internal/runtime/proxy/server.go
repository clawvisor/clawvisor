package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
)

type Config struct {
	DataDir           string
	Addr              string
	TLS               bool
	ListenerHostnames []string
	LeafCacheSize     int
}

type Server struct {
	cfg      Config
	logger   *slog.Logger
	ca       *x509.Certificate
	caKey    *ecdsa.PrivateKey
	certs    *LeafCertCache
	goproxy  *goproxy.ProxyHttpServer
	listener net.Listener
	httpSrv  *http.Server

	connStates                sync.Map
	latestRequestCtxBySession sync.Map
	latestRequestCtxPruneTick uint64
	secretValueCache          sync.Map
	secretVerdictCache        sync.Map
}

func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("runtime proxy data dir is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:25290"
	}
	ca, caKey, err := LoadOrGenerateCA(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	certs := NewLeafCertCache(ca, caKey, cfg.LeafCacheSize)

	p := goproxy.NewProxyHttpServer()
	p.Verbose = false
	p.CertStore = &goproxyCertAdapter{cache: certs}

	s := &Server{
		cfg:     cfg,
		logger:  logger,
		ca:      ca,
		caKey:   caKey,
		certs:   certs,
		goproxy: p,
	}
	return s, nil
}

func (s *Server) GoProxy() *goproxy.ProxyHttpServer { return s.goproxy }

func (s *Server) CA() *x509.Certificate { return s.ca }

func (s *Server) Addr() string { return s.cfg.Addr }

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("runtime proxy listen %s: %w", s.cfg.Addr, err)
	}
	s.listener = ln
	s.cfg.Addr = ln.Addr().String()

	s.httpSrv = &http.Server{
		Handler:           s.goproxy,
		ReadHeaderTimeout: 30 * time.Second,
		ConnState: func(c net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				s.connStates.Delete(c)
			} else {
				s.connStates.Store(c, state)
			}
		},
	}

	if s.cfg.TLS {
		tlsCfg, err := s.listenerTLSConfig()
		if err != nil {
			_ = ln.Close()
			return err
		}
		s.httpSrv.TLSConfig = tlsCfg
		go func() { _ = s.httpSrv.ServeTLS(ln, "", "") }()
	} else {
		go func() { _ = s.httpSrv.Serve(ln) }()
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

func (s *Server) listenerTLSConfig() (*tls.Config, error) {
	names := s.cfg.ListenerHostnames
	if len(names) == 0 {
		names = []string{"localhost", "127.0.0.1"}
	}
	primary := names[0]
	getCert := func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		host := chi.ServerName
		if host == "" {
			host = primary
		}
		return s.certs.Get(host)
	}
	if _, err := s.certs.Get(primary); err != nil {
		return nil, fmt.Errorf("pre-mint listener cert: %w", err)
	}
	return &tls.Config{
		GetCertificate: getCert,
		MinVersion:     tls.VersionTLS12,
	}, nil
}

type goproxyCertAdapter struct{ cache *LeafCertCache }

func (a *goproxyCertAdapter) Fetch(hostname string, _ func() (*tls.Certificate, error)) (*tls.Certificate, error) {
	return a.cache.Get(hostname)
}
