package grpc_rest_server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Falokut/interceptors"
	"github.com/opentracing/opentracing-go"
	"github.com/sirupsen/logrus"
	"github.com/soheilhy/cmux"

	grpcrecovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type server struct {
	service                any
	logger                 *logrus.Logger
	grpcServer             *grpc.Server
	restServer             *http.Server
	AllowedHeaders         []string
	AllowedOutgoingHeaders map[string]string
	im                     *interceptors.InterceptorManager
	mux                    cmux.CMux
	errCh                  chan error
}

func NewServer(logger *logrus.Logger, service any) server {
	errCh := make(chan error)
	return server{logger: logger, service: service, errCh: errCh}
}

type Config struct {
	Host                   string
	Port                   string
	Mode                   string
	AllowedHeaders         []string
	AllowedOutgoingHeaders map[string]string

	ServiceDesc               *grpc.ServiceDesc
	RegisterRestHandlerServer func(ctx context.Context, mux *runtime.ServeMux, service any) error
	// By default 4 mb
	MaxRequestSize int
	// By default 4 mb
	MaxResponceSize int
	// Cert == nil, then insecure credentials will be used, only for grpc
	Cert *tls.Certificate
	// Used for REST
	TlsConfig *tls.Config
}

func (s *server) Run(cfg Config, metric interceptors.Metrics,
	grpcOptions *[]grpc.ServerOption, customRestMux *runtime.ServeMux) error {
	Mode := strings.ToUpper(cfg.Mode)
	s.logger.Info("start running server on mode: " + Mode)
	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%s", cfg.Host, cfg.Port))
	if err != nil {
		s.logger.Error("error while creating listener ", err)
		return err
	}
	s.mux = cmux.New(lis)
	s.im = interceptors.NewInterceptorManager(s.logger, metric)
	s.AllowedHeaders = cfg.AllowedHeaders
	s.AllowedOutgoingHeaders = cfg.AllowedOutgoingHeaders

	if cfg.MaxRequestSize <= 0 {
		cfg.MaxRequestSize = 4 * mb
	}
	if cfg.MaxResponceSize <= 0 {
		cfg.MaxResponceSize = 4 * mb
	}

	switch Mode {
	case "REST":
		s.runRestAPI(cfg, customRestMux)
	case "GRPC":
		s.runGRPC(cfg, grpcOptions)
		grpc_prometheus.Register(s.grpcServer)
	case "BOTH":
		s.runRestAPI(cfg, customRestMux)
		s.runGRPC(cfg, grpcOptions)
		grpc_prometheus.Register(s.grpcServer)
	}
	if err := s.mux.Serve(); err != nil {
		s.logger.Errorf("error while mux serving %s", err)
		return err
	}

	s.logger.Info("server running on mode: " + Mode)
	return <-s.errCh
}

func (s *server) GetDefaultOptions() *[]grpc.ServerOption {
	return &[]grpc.ServerOption{grpc.UnaryInterceptor(s.im.Logger), grpc.ChainUnaryInterceptor(
		otgrpc.OpenTracingServerInterceptor(opentracing.GlobalTracer()),
		grpc_ctxtags.UnaryServerInterceptor(),
		s.im.Metrics,
		grpcrecovery.UnaryServerInterceptor(),
	), grpc.StreamInterceptor(s.im.StreamLogger), grpc.ChainStreamInterceptor(
		otgrpc.OpenTracingStreamServerInterceptor(opentracing.GlobalTracer()),
		grpc_ctxtags.StreamServerInterceptor(),
		s.im.StreamMetrics,
		grpcrecovery.StreamServerInterceptor(),
	),
	}
}

const (
	kb = 8 << 10
	mb = kb << 10
)

// in server options mustn't be MaxRecvMsgSize,MaxSendMsgSize and any credentials, specify params in config
func (s *server) runGRPC(cfg Config, serverOptions *[]grpc.ServerOption) {
	s.logger.Info("GRPC server initializing")

	var creds grpc.ServerOption
	if cfg.Cert == nil {
		creds = grpc.Creds(insecure.NewCredentials())
	} else {
		grpc.Creds(credentials.NewServerTLSFromCert(cfg.Cert))
	}
	if serverOptions == nil || len(*serverOptions) == 0 {
		serverOptions = s.GetDefaultOptions()
	}

	*serverOptions = append(*serverOptions, creds,
		grpc.MaxRecvMsgSize(cfg.MaxRequestSize),
		grpc.MaxSendMsgSize(cfg.MaxResponceSize))
	s.grpcServer = grpc.NewServer(*serverOptions...)

	s.grpcServer.RegisterService(cfg.ServiceDesc, s.service)
	go func() {
		grpcL := s.mux.Match(cmux.HTTP2HeaderField("content-type", "application/grpc"))
		if err := s.grpcServer.Serve(grpcL); err != nil {
			s.logger.Errorf("GRPC error while serving: %v", err)
			s.errCh <- err
			return
		}
	}()
	s.logger.Infof("GRPC server initialized. Listen on %s:%s", cfg.Host, cfg.Port)
}

func (s *server) runRestAPI(cfg Config, mux *runtime.ServeMux) {
	s.logger.Info("REST server initializing")

	if mux == nil {
		mux = runtime.NewServeMux(
			runtime.WithIncomingHeaderMatcher(s.headerMatcherFunc),
			runtime.WithOutgoingHeaderMatcher(s.outgoingHeaderMatcher),
		)
	}

	if err := cfg.RegisterRestHandlerServer(context.Background(), mux, s.service); err != nil {
		s.logger.Errorf("REST server error while registering handler server: %v", err)
		s.errCh <- err
		return
	}

	s.restServer = &http.Server{
		Handler:   s.im.RestTracer(s.im.RestLogger(s.im.RestMetrics(mux))),
		TLSConfig: cfg.TlsConfig,
	}

	s.logger.Info("Rest server initializing")
	go func() {
		restL := s.mux.Match(cmux.HTTP1Fast())

		if err := s.restServer.Serve(restL); err != nil {
			s.logger.Errorf("REST server error while serving: %v", err)
			s.errCh <- err
			return
		}
	}()

	s.logger.Infof("REST server initialized. Listen on %s:%s", cfg.Host, cfg.Port)
}

func (s *server) Shutdown() {
	s.logger.Println("Shutting down")
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
	if s.restServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()
		err := s.restServer.Shutdown(ctx)
		if err != nil {
			s.logger.Errorf("error while shutting down rest server: %v", err)
		}
	}
	s.mux.Close()
}

func (s *server) headerMatcherFunc(header string) (string, bool) {
	s.logger.Debugf("Received %s header", header)
	for _, AllowedHeader := range s.AllowedHeaders {
		if header == AllowedHeader {
			return AllowedHeader, true
		}
	}

	return runtime.DefaultHeaderMatcher(header)
}

func (s *server) outgoingHeaderMatcher(header string) (string, bool) {
	s.logger.Debugf("Outgoing %s header", header)
	for preatyHeaderName, AllowedHeader := range s.AllowedOutgoingHeaders {
		if header == AllowedHeader {
			return preatyHeaderName, true
		}
	}
	return runtime.DefaultHeaderMatcher(header)
}
