package grpc_rest_server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

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
	"google.golang.org/grpc/credentials/insecure"
)

type server struct {
	service                any
	logger                 *logrus.Logger
	grpcServer             *grpc.Server
	AllowedHeaders         []string
	AllowedOutgoingHeaders map[string]string
	im                     *interceptors.InterceptorManager
	mux                    cmux.CMux
}

func NewServer(logger *logrus.Logger, service any) server {
	return server{logger: logger, service: service}
}

type Config struct {
	Host                   string
	Port                   string
	Mode                   string
	AllowedHeaders         []string
	AllowedOutgoingHeaders map[string]string

	ServiceDesc               *grpc.ServiceDesc
	RegisterRestHandlerServer func(ctx context.Context, mux *runtime.ServeMux, service any) error
}

func (s *server) Run(cfg Config, metric interceptors.Metrics, grpcOptions *[]grpc.ServerOption, customRestMux *runtime.ServeMux) {
	Mode := strings.ToUpper(cfg.Mode)
	s.logger.Info("start running server on mode: " + Mode)
	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%s", cfg.Host, cfg.Port))
	if err != nil {
		s.logger.Fatal("error while listening", err)
	}
	s.mux = cmux.New(lis)
	s.im = interceptors.NewInterceptorManager(s.logger, metric)
	s.AllowedHeaders = cfg.AllowedHeaders
	s.AllowedOutgoingHeaders = cfg.AllowedOutgoingHeaders

	switch Mode {
	case "REST":
		s.runRestAPI(cfg, customRestMux)
	case "GRPC":
		s.runGRPC(cfg, grpcOptions)
	case "BOTH":
		s.runRestAPI(cfg, customRestMux)
		s.runGRPC(cfg, grpcOptions)
	}
	if err := s.mux.Serve(); err != nil {
		s.logger.Fatal(err)
	}
	grpc_prometheus.Register(s.grpcServer)

	s.logger.Info("server running on mode: " + Mode)
}

func (s *server) runGRPC(cfg Config, serverOptions *[]grpc.ServerOption) {
	s.logger.Info("GRPC server initializing")

	if serverOptions == nil || len(*serverOptions) < 1 {
		serverOptions = &[]grpc.ServerOption{grpc.UnaryInterceptor(s.im.Logger), grpc.ChainUnaryInterceptor(
			otgrpc.OpenTracingServerInterceptor(opentracing.GlobalTracer()),
			grpc_ctxtags.UnaryServerInterceptor(),
			s.im.Metrics,
			grpcrecovery.UnaryServerInterceptor(),
		), grpc.StreamInterceptor(s.im.StreamLogger), grpc.ChainStreamInterceptor(
			otgrpc.OpenTracingStreamServerInterceptor(opentracing.GlobalTracer()),
			grpc_ctxtags.StreamServerInterceptor(),
			s.im.StreamMetrics,
			grpcrecovery.StreamServerInterceptor(),
		), grpc.Creds(insecure.NewCredentials()),
		}
	}

	s.grpcServer = grpc.NewServer(*serverOptions...)

	s.grpcServer.RegisterService(cfg.ServiceDesc, s.service)
	go func() {
		grpcL := s.mux.Match(cmux.HTTP2HeaderField("content-type", "application/grpc"))
		if err := s.grpcServer.Serve(grpcL); err != nil {
			s.logger.Fatalf("GRPC error while serving: %v", err)
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
		s.logger.Fatalf("REST server error while registering handler server: %v", err)

	}

	server := http.Server{
		Handler: s.im.RestLogger(s.im.RestMetrics(mux)),
	}

	s.logger.Info("Rest server initializing")
	go func() {
		restL := s.mux.Match(cmux.HTTP1Fast())

		if err := server.Serve(restL); err != nil {
			s.logger.Fatalf("REST server error while serving: %v", err)
		}
	}()

	s.logger.Infof("REST server initialized. Listen on %s:%s", cfg.Host, cfg.Port)
}

func (s *server) Shutdown() {
	s.logger.Println("Shutting down")
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
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
