// This file registers the gRPC service plugin with the Lynx global plugin factory
// and exposes GetGrpcServer as a convenience helper for retrieving the running server instance.
package grpc

import (
	"fmt"

	"github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/go-lynx/lynx"
	"github.com/go-lynx/lynx/pkg/factory"
	"github.com/go-lynx/lynx/plugins"
)

// init registers the gRPC service plugin with the global plugin factory on import.
func init() {
	factory.GlobalTypedFactory().RegisterPlugin(pluginName, confPrefix, func() plugins.Plugin {
		return NewGrpcService()
	})
}

// GetGrpcServer returns the running gRPC server so callers can register services on it.
// Pass nil to use the global Lynx plugin manager. Errors if the plugin is missing or
// the server has not started yet.
func GetGrpcServer(pluginManager any) (*grpc.Server, error) {
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
