package grpc

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	kratosmiddleware "github.com/go-kratos/kratos/v2/middleware"
	lynxapp "github.com/go-lynx/lynx"
	"github.com/go-lynx/lynx-grpc/conf"
	grpcgo "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

type grpcServiceControlPlaneRateLimiter struct {
	unaryHits atomic.Int32
}

func (g *grpcServiceControlPlaneRateLimiter) HTTPRateLimit() kratosmiddleware.Middleware {
	return nil
}

func (g *grpcServiceControlPlaneRateLimiter) GRPCRateLimit() kratosmiddleware.Middleware {
	return func(next kratosmiddleware.Handler) kratosmiddleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			g.unaryHits.Add(1)
			return next(ctx, req)
		}
	}
}

type grpcServiceControlPlaneTestServer interface {
	Ping(context.Context, *emptypb.Empty) (*emptypb.Empty, error)
}

type grpcServiceControlPlanePingService struct{}

func (grpcServiceControlPlanePingService) Ping(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

var grpcServiceControlPlanePingServiceDesc = grpcgo.ServiceDesc{
	ServiceName: "lynx.grpc.test.ControlPlaneService",
	HandlerType: (*grpcServiceControlPlaneTestServer)(nil),
	Methods: []grpcgo.MethodDesc{
		{
			MethodName: "Ping",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpcgo.UnaryServerInterceptor) (interface{}, error) {
				in := new(emptypb.Empty)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(grpcServiceControlPlaneTestServer).Ping(ctx, in)
				}
				info := &grpcgo.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/lynx.grpc.test.ControlPlaneService/Ping",
				}
				handler := func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(grpcServiceControlPlaneTestServer).Ping(ctx, req.(*emptypb.Empty))
				}
				return interceptor(ctx, in, info, handler)
			},
		},
	},
}

func TestGrpcService_StartupTasks_AttachesControlPlaneRateLimiterToUnaryRequests(t *testing.T) {
	plugin := NewGrpcService()
	plane := &grpcServiceControlPlaneRateLimiter{}
	plugin.SetDependencies(nil, nil, nil, func() interface{} { return plane })
	plugin.conf = &conf.Service{
		Network: "tcp",
		Addr:    "127.0.0.1:0",
		Timeout: durationpb.New(2 * time.Second),
	}
	plugin.serverOpts = buildServerOptionsFromConfig(plugin.conf, defaultServerOptions())

	if err := plugin.StartupTasks(); err != nil {
		t.Fatalf("StartupTasks failed: %v", err)
	}

	plugin.server.Server.RegisterService(&grpcServiceControlPlanePingServiceDesc, grpcServiceControlPlanePingService{})

	endpoint, err := plugin.server.Endpoint()
	if err != nil {
		t.Fatalf("resolve endpoint: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- plugin.server.Start(context.Background())
	}()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelDial()
	conn, err := grpcgo.DialContext(dialCtx, endpoint.Host, grpcgo.WithTransportCredentials(insecure.NewCredentials()), grpcgo.WithBlock())
	if err != nil {
		_ = plugin.CleanupTasksContext(context.Background())
		t.Fatalf("dial gRPC server: %v", err)
	}

	invokeCtx, cancelInvoke := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelInvoke()
	if err := conn.Invoke(invokeCtx, "/lynx.grpc.test.ControlPlaneService/Ping", &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
		_ = conn.Close()
		_ = plugin.CleanupTasksContext(context.Background())
		t.Fatalf("invoke Ping RPC: %v", err)
	}

	if got := plane.unaryHits.Load(); got != 1 {
		_ = conn.Close()
		_ = plugin.CleanupTasksContext(context.Background())
		t.Fatalf("control-plane gRPC rate limiter hits = %d, want 1", got)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close client conn: %v", err)
	}
	if err := plugin.CleanupTasksContext(context.Background()); err != nil {
		t.Fatalf("cleanup gRPC service: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("server start returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gRPC server start goroutine did not exit after cleanup")
	}
}

func TestGrpcService_GetGRPCRateLimit_FallsBackToDefaultAppControlPlane(t *testing.T) {
	lynxapp.ClearDefaultApp()
	t.Cleanup(lynxapp.ClearDefaultApp)

	app := new(lynxapp.LynxApp)
	plane := &grpcServiceControlPlaneRateLimiter{}
	if err := app.SetRateLimiter(plane); err != nil {
		t.Fatalf("set rate limiter capability: %v", err)
	}
	lynxapp.SetDefaultApp(app)

	plugin := NewGrpcService()
	if plugin.getGRPCRateLimit() == nil {
		t.Fatal("expected gRPC rate limit middleware from default app control plane fallback")
	}
}
