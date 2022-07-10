// Copyright 2022 NLP Odyssey Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/rs/cors"
	"github.com/rs/zerolog/log"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

const (
	// DefaultNetwork is the default network.
	DefaultNetwork = "tcp4"
	// DefaultAddress is the default address.
	DefaultAddress = ":8080"
)

// Server is a server that provides gRPC and HTTP/2 APIs.
type Server struct {
	conf       *Config
	taskServer TaskServer
}

// Config is the configuration for the server.
type Config struct {
	Network        string
	Address        string
	AllowedOrigins []string
	TLSEnabled     bool
	TLSCert        string
	TLSKey         string
}

// TaskServer is implemented by any task-specific service that can be
// registered in the main Server.
type TaskServer interface {
	RegisterServer(grpc.ServiceRegistrar) error
	RegisterHandlerServer(context.Context, *runtime.ServeMux) error
}

// New creates a new server.
func New(conf *Config, taskServer TaskServer) *Server {
	setBaselineConfig(conf)
	return &Server{
		conf:       conf,
		taskServer: taskServer,
	}
}

func setBaselineConfig(c *Config) {
	if c.Network == "" {
		c.Network = DefaultNetwork
	}
	if c.Address == "" {
		c.Address = DefaultAddress
	}
}

// Start up the server, this will block.
// Start via a Go routine if needed.
func (s *Server) Start(ctx context.Context) {
	conf := s.conf

	grpcServer := grpc.NewServer()

	healthCheck := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthCheck)

	if err := s.taskServer.RegisterServer(grpcServer); err != nil {
		log.Fatal().Err(fmt.Errorf("failed to register gRPC server: %w", err)).Send()
	}

	gwMux := runtime.NewServeMux()
	if err := s.taskServer.RegisterHandlerServer(ctx, gwMux); err != nil {
		log.Fatal().Err(fmt.Errorf("failed to register gRPC handler server: %w", err)).Send()
	}

	mux := gwMux

	lis, err := net.Listen(conf.Network, conf.Address)
	if err != nil {
		log.Fatal().Err(fmt.Errorf("failed to listen on %s (%s): %w", conf.Address, conf.Network, err)).Send()
	}

	if strings.HasSuffix(conf.Address, ":0") {
		conf.Address = lis.Addr().String()
	}

	handler := cors.New(s.corsOptions()).Handler(mux)
	handler = s.handlerFunc(grpcServer, handler)

	go runHealthCheckLoop(healthCheck)

	if conf.TLSEnabled {
		s.serveTLS(ctx, lis, handler)
	}
	s.serveInsecure(ctx, lis, handler)
}

const (
	healthCheckSleep         = 5 * time.Second
	healthCheckSystemService = "" // empty string represents the system, rather than a specific service
)

func runHealthCheckLoop(healthCheck *health.Server) {
	next := grpc_health_v1.HealthCheckResponse_SERVING
	for {
		healthCheck.SetServingStatus(healthCheckSystemService, next)

		if next == grpc_health_v1.HealthCheckResponse_SERVING {
			next = grpc_health_v1.HealthCheckResponse_NOT_SERVING
		} else {
			next = grpc_health_v1.HealthCheckResponse_SERVING
		}

		time.Sleep(healthCheckSleep)
	}
}

// corsOptions returns the CORS options for the server.
func (s *Server) corsOptions() cors.Options {
	return cors.Options{
		AllowedOrigins: s.conf.AllowedOrigins,
	}
}

// handlerFunc returns a handler that adds the gRPC server to the HTTP/2 server.
func (s *Server) handlerFunc(grpcServer *grpc.Server, httpHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isGRPCRequest(r) {
			grpcServer.ServeHTTP(w, r)
		} else {
			httpHandler.ServeHTTP(w, r)
		}
	})
}

// isGRPCRequest returns true if the request is a gRPC request.
func isGRPCRequest(r *http.Request) bool {
	return r.ProtoMajor == 2 &&
		strings.Contains(r.Header.Get("Content-Type"), "application/grpc")
}

// serveTLS starts the server with TLS.
func (s *Server) serveTLS(ctx context.Context, lis net.Listener, handler http.Handler) {
	conf := s.conf

	tlsCert, err := tls.LoadX509KeyPair(conf.TLSCert, conf.TLSKey)
	if err != nil {
		log.Fatal().Err(fmt.Errorf("failed to load TLS public/private key pair: %w", err)).Send()
	}

	hs := &http.Server{
		Handler: handler,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			NextProtos:   []string{"h2"},
		},
	}

	log.Info().Str("network", conf.Network).Str("address", conf.Address).Bool("TLS", conf.TLSEnabled).Msg("server listening")

	go shutDownServerWhenContextIsDone(ctx, hs)

	err = hs.Serve(tls.NewListener(lis, hs.TLSConfig))
	if err != nil {
		log.Fatal().Err(fmt.Errorf("server error: %w", err)).Send()
	}
}

// serveInsecure starts the server without TLS.
func (s *Server) serveInsecure(ctx context.Context, lis net.Listener, handler http.Handler) {
	conf := s.conf

	h2s := &http2.Server{}
	h1s := &http.Server{
		Handler: h2c.NewHandler(handler, h2s),
	}

	log.Info().Str("network", conf.Network).Str("address", conf.Address).Bool("TLS", conf.TLSEnabled).Msg("server listening")

	go shutDownServerWhenContextIsDone(ctx, h1s)

	err := h1s.Serve(lis)
	if err != nil {
		log.Fatal().Err(fmt.Errorf("server error: %w", err)).Send()
	}
}

// shutDownServerWhenContextIsDone shuts down the server when the context is done.
func shutDownServerWhenContextIsDone(ctx context.Context, hs *http.Server) {
	<-ctx.Done()
	log.Info().Msg("context done, shutting down server")
	err := hs.Shutdown(context.Background())
	if err != nil {
		log.Err(err).Msg("server shutdown error")
	}
}

// ReadyForConnections returns `true` if the server is ready to accept requests.
// If after the duration `dur` the server is still not ready, returns `false`.
func (s *Server) ReadyForConnections(dur time.Duration) bool {
	return s.readyForConnections(dur) == nil
}

func (s *Server) readyForConnections(d time.Duration) error {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if err := s.check(); err == nil {
			return nil
		}
		if d > 25*time.Millisecond {
			time.Sleep(25 * time.Millisecond)
		}
	}
	return fmt.Errorf("failed to be ready for connections after %s", d)
}

// check checks if the server is ready for connections.
func (s *Server) check() error {
	conn, err := net.Dial(s.conf.Network, s.conf.Address)
	if err != nil {
		return fmt.Errorf("failed to connect to %s (%s): %w", s.conf.Address, s.conf.Network, err)
	}
	conn.Close()
	return nil
}

// ClientAddr returns the Address used to connect clients (without the network).
// Helpful in testing when we designate a random port (0).
func (s *Server) ClientAddr() string {
	return s.conf.Address
}
