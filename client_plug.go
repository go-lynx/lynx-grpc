package grpc

import (
	"fmt"

	"github.com/go-lynx/lynx"
	"github.com/go-lynx/lynx/pkg/factory"
	"github.com/go-lynx/lynx/plugins"
	"google.golang.org/grpc"
)

const clientPluginName = "grpc.client"

// init function registers the gRPC client plugin to the global plugin factory
func init() {
	factory.GlobalTypedFactory().RegisterPlugin(clientPluginName, "lynx.grpc.client", func() plugins.Plugin {
		return NewGrpcClientPlugin()
	})
}

// GetGrpcClientPlugin retrieves the gRPC client plugin from the plugin manager.
// When pluginManager is provided (e.g. lynx.Lynx().GetPluginManager()), returns the registered plugin instance.
// When pluginManager is nil, falls back to lynx.Lynx().GetPluginManager() if available.
func GetGrpcClientPlugin(pluginManager interface{}) (*ClientPlugin, error) {
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

	p := pm.GetPlugin(clientPluginName)
	if p == nil {
		return nil, fmt.Errorf("plugin %s not found", clientPluginName)
	}
	clientPlugin, ok := p.(*ClientPlugin)
	if !ok {
		return nil, fmt.Errorf("plugin %s is not a *ClientPlugin instance", clientPluginName)
	}
	return clientPlugin, nil
}

// GetOrCreateGrpcClientPlugin gets existing plugin or creates a new one
func GetOrCreateGrpcClientPlugin() *ClientPlugin {
	plugin, err := factory.GlobalTypedFactory().CreatePlugin("grpc.client")
	if err == nil {
		if clientPlugin, ok := plugin.(*ClientPlugin); ok {
			return clientPlugin
		}
	}

	// Create new plugin if not found
	return NewGrpcClientPlugin()
}

// GetGrpcClientConnection gets a gRPC client connection for the specified service
func GetGrpcClientConnection(serviceName string, pluginManager interface{}) (*grpc.ClientConn, error) {
	plugin, err := GetGrpcClientPlugin(pluginManager)
	if err != nil {
		return nil, err
	}

	return plugin.GetConnection(serviceName)
}

// CreateGrpcClientConnection creates a new gRPC client connection with custom configuration
func CreateGrpcClientConnection(config ClientConfig, pluginManager interface{}) (*grpc.ClientConn, error) {
	plugin, err := GetGrpcClientPlugin(pluginManager)
	if err != nil {
		return nil, err
	}

	return plugin.CreateConnection(config)
}

// CloseGrpcClientConnection closes a gRPC client connection
func CloseGrpcClientConnection(serviceName string, pluginManager interface{}) error {
	plugin, err := GetGrpcClientPlugin(pluginManager)
	if err != nil {
		return err
	}

	return plugin.connectionPool.CloseConnection(serviceName)
}

// GetGrpcClientConnectionStatus returns the status of all gRPC client connections
func GetGrpcClientConnectionStatus(pluginManager interface{}) (map[string]string, error) {
	plugin, err := GetGrpcClientPlugin(pluginManager)
	if err != nil {
		return nil, err
	}

	return plugin.GetConnectionStatus(), nil
}

// GetGrpcClientConnectionCount returns the number of active gRPC client connections
func GetGrpcClientConnectionCount(pluginManager interface{}) (int, error) {
	plugin, err := GetGrpcClientPlugin(pluginManager)
	if err != nil {
		return 0, err
	}

	return plugin.GetConnectionCount(), nil
}
