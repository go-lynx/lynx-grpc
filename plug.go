package grpc

import (
	"fmt"
	"github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/go-lynx/lynx"
	"github.com/go-lynx/lynx/pkg/factory"
	"github.com/go-lynx/lynx/plugins"
)

// init function registers the gRPC service plugin to the global plugin factory.
// This function is automatically called when the package is imported.
// It creates a new Service instance and registers it to the package grpc

func init() {
	// Call the RegisterPlugin method of the global plugin factory for plugin registration
	// Pass in the plugin name, configuration prefix, and a function that returns a plugins.Plugin interface instance
	factory.GlobalTypedFactory().RegisterPlugin(pluginName, confPrefix, func() plugins.Plugin {
		// Create and return a new Service instance
		return NewGrpcService()
	})
}

// GetGrpcServer gets the gRPC server instance from the plugin manager.
// This function provides access to the underlying gRPC server for other parts of the application
// that may need to register services or use server functionality.
//
// Returns:
//   - *grpc.Server: Configured gRPC server instance
//   - error: Any error that occurred while retrieving the server
func GetGrpcServer(pluginManager interface{}) (*grpc.Server, error) {
	var pm lynx.PluginManager
	if pluginManager == nil {
		if lynx.Lynx() == nil || lynx.Lynx().GetPluginManager() == nil {
			return nil, fmt.Errorf("plugin manager is nil")
		}
		pm = lynx.Lynx().GetPluginManager()
	} else {
		var ok bool
		pm, ok = pluginManager.(lynx.PluginManager)
		if !ok {
			return nil, fmt.Errorf("unsupported plugin manager type %T", pluginManager)
		}
	}

	p := pm.GetPlugin(pluginName)
	if p == nil {
		return nil, fmt.Errorf("plugin %s not found", pluginName)
	}
	svc, ok := p.(*Service)
	if !ok {
		return nil, fmt.Errorf("plugin %s is not a *Service instance", pluginName)
	}
	if svc.server == nil {
		return nil, fmt.Errorf("gRPC server not initialized")
	}
	return svc.server, nil
}
