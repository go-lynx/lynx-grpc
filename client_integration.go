// This file provides ClientIntegration, a higher-level helper that bridges the gRPC client plugin
// with the Lynx app subscription model, building connections from app-level configuration.
package grpc

import (
	"fmt"
	"time"

	"github.com/go-kratos/kratos/v2/registry"
	"github.com/go-kratos/kratos/v2/selector"
	"github.com/go-lynx/lynx/conf"
	"github.com/go-lynx/lynx/log"
	"google.golang.org/grpc"
)

// ClientIntegration provides integration with app/subscribe
type ClientIntegration struct {
	clientPlugin  *ClientPlugin
	discovery     registry.Discovery
	routerFactory func(string) selector.NodeFilter
}

// NewGrpcClientIntegration creates a new gRPC client integration
func NewGrpcClientIntegration(discovery registry.Discovery, routerFactory func(string) selector.NodeFilter) *ClientIntegration {
	return &ClientIntegration{
		discovery:     discovery,
		routerFactory: routerFactory,
	}
}

// SetClientPlugin sets the gRPC client plugin
func (g *ClientIntegration) SetClientPlugin(plugin *ClientPlugin) {
	g.clientPlugin = plugin
}

// BuildGrpcSubscriptions builds gRPC subscription connections using the client plugin
func (g *ClientIntegration) BuildGrpcSubscriptions(cfg *conf.Subscriptions) (map[string]*grpc.ClientConn, error) {
	if g.clientPlugin == nil {
		return nil, fmt.Errorf("gRPC client plugin not initialized")
	}

	conns := make(map[string]*grpc.ClientConn)

	if cfg == nil || len(cfg.GetGrpc()) == 0 {
		return conns, nil
	}

	for _, item := range cfg.GetGrpc() {
		name := item.GetService()
		if name == "" {
			log.Warnf("skip empty grpc subscription entry")
			continue
		}

		clientConfig := ClientConfig{
			ServiceName: name,
			Discovery:   g.discovery,
			TLS:         item.GetTls(),
			Timeout:     g.clientPlugin.defaultTimeout(),
			KeepAlive:   g.clientPlugin.defaultKeepAlive(),
			Middleware:  g.clientPlugin.getDefaultMiddleware(),
		}

		if g.routerFactory != nil {
			clientConfig.NodeFilter = g.routerFactory(name)
		}

		if item.GetTls() {
			clientConfig.TLS = true
		}

		conn, err := g.clientPlugin.CreateConnection(clientConfig)
		if err != nil {
			if item.GetRequired() {
				return nil, fmt.Errorf("required grpc subscription failed: %s, error: %v", name, err)
			}
			log.Warnf("grpc subscription created failed: %s, error: %v", name, err)
			continue
		}

		if conn == nil {
			if item.GetRequired() {
				return nil, fmt.Errorf("required grpc subscription failed: %s", name)
			}
			log.Warnf("grpc subscription created nil conn: %s", name)
			continue
		}

		state := conn.GetState()
		log.Infof("grpc subscription established: service=%s state=%s", name, state.String())

		conns[name] = conn
	}

	return conns, nil
}

// GetConnection gets a connection for a specific service
func (g *ClientIntegration) GetConnection(serviceName string) (*grpc.ClientConn, error) {
	if g.clientPlugin == nil {
		return nil, fmt.Errorf("gRPC client plugin not initialized")
	}

	return g.clientPlugin.GetConnection(serviceName)
}

// CloseConnection closes a connection for a specific service
func (g *ClientIntegration) CloseConnection(serviceName string) error {
	if g.clientPlugin == nil {
		return fmt.Errorf("gRPC client plugin not initialized")
	}

	return g.clientPlugin.CloseServiceConnection(serviceName)
}

func (c *ClientPlugin) defaultTimeout() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.conf != nil && c.conf.DefaultTimeout != nil {
		if d := c.conf.DefaultTimeout.AsDuration(); d > 0 {
			return d
		}
	}
	return 10 * time.Second
}

func (c *ClientPlugin) defaultKeepAlive() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.conf != nil && c.conf.DefaultKeepAlive != nil {
		if d := c.conf.DefaultKeepAlive.AsDuration(); d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

// GetConnectionStatus returns the status of all connections
func (g *ClientIntegration) GetConnectionStatus() map[string]string {
	if g.clientPlugin == nil {
		return make(map[string]string)
	}

	return g.clientPlugin.GetConnectionStatus()
}

// GetConnectionCount returns the number of active connections
func (g *ClientIntegration) GetConnectionCount() int {
	if g.clientPlugin == nil {
		return 0
	}

	return g.clientPlugin.GetConnectionCount()
}

// HealthCheck performs health check on all connections
func (g *ClientIntegration) HealthCheck() error {
	if g.clientPlugin == nil {
		return fmt.Errorf("gRPC client plugin not initialized")
	}

	return g.clientPlugin.CheckHealth()
}

// GetMetrics returns client metrics
func (g *ClientIntegration) GetMetrics() *ClientMetrics {
	if g.clientPlugin == nil {
		return nil
	}

	return g.clientPlugin.metrics
}
