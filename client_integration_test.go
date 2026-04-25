package grpc

import (
	"testing"

	grpcconf "github.com/go-lynx/lynx-grpc/conf"
	lynxconf "github.com/go-lynx/lynx/conf"
	"google.golang.org/grpc"
)

func TestClientIntegration_BuildSubscriptionsUsesSafeDurationDefaults(t *testing.T) {
	plugin := NewGrpcClientPlugin()
	plugin.conf = &grpcconf.GrpcClient{}
	plugin.connectionPool = NewConnectionPool(10, 5, 0, false, plugin.metrics)

	integration := NewGrpcClientIntegration(nil, nil)
	integration.SetClientPlugin(plugin)

	conns, err := integration.BuildGrpcSubscriptions(&lynxconf.Subscriptions{
		Grpc: []*lynxconf.GrpcSubscription{
			{Service: "optional-service"},
		},
	})
	if err != nil {
		t.Fatalf("BuildGrpcSubscriptions returned error for optional service: %v", err)
	}
	if len(conns) != 0 {
		t.Fatalf("expected no connections for unresolved optional service, got %d", len(conns))
	}
}

func TestClientIntegration_CloseConnectionWithoutPool(t *testing.T) {
	plugin := &ClientPlugin{
		conf:        &grpcconf.GrpcClient{},
		connections: make(map[string]*grpc.ClientConn),
	}
	integration := NewGrpcClientIntegration(nil, nil)
	integration.SetClientPlugin(plugin)

	if err := integration.CloseConnection("svc"); err != nil {
		t.Fatalf("CloseConnection should tolerate nil connection pool: %v", err)
	}
}
