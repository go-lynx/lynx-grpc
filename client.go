// This file implements the gRPC client plugin (ClientPlugin) for the Lynx framework,
// including connection management, service discovery, TLS, retry, and health checking.
package grpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/middleware/logging"
	"github.com/go-kratos/kratos/v2/middleware/tracing"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/go-kratos/kratos/v2/selector"
	"github.com/go-lynx/lynx"
	"github.com/go-lynx/lynx-grpc/conf"
	"github.com/go-lynx/lynx/plugins"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// ClientPlugin represents the gRPC client plugin
type ClientPlugin struct {
	*plugins.BasePlugin
	conf            *conf.GrpcClient
	connections     map[string]*grpc.ClientConn
	connectionPool  *ConnectionPool
	loadBalancer    *LoadBalancer
	circuitBreakers *CircuitBreakerManager
	discovery       registry.Discovery
	tlsManager      *TLSManager
	mu              sync.RWMutex
	metrics         *ClientMetrics
	rt              plugins.Runtime
}

var serviceDiscoverySharedResourceNames = []string{
	"polaris.control.plane.service_discovery",
	"nacos.control.plane.service_discovery",
}

// publishRequiredReadiness publishes the required-upstreams readiness state to the shared runtime resource.
func (c *ClientPlugin) publishRequiredReadiness(ready bool) {
	if c == nil || c.rt == nil {
		return
	}
	for _, resourceName := range []string{requiredReadinessResourceName, requiredReadinessStableResourceName} {
		if err := c.rt.RegisterSharedResource(resourceName, ready); err != nil {
			log.Warnf("Failed to publish required readiness state %s: %v", resourceName, err)
		}
	}
	if err := c.rt.RegisterPrivateResource(requiredReadinessPrivateResourceName, ready); err != nil {
		log.Warnf("Failed to publish private required readiness state: %v", err)
	}
	if ready {
		log.Infof("Published required upstream readiness: READY")
	} else {
		log.Warnf("Published required upstream readiness: NOT READY")
	}
}

// ClientConfig represents configuration for a specific gRPC client connection
type ClientConfig struct {
	ServiceName    string
	Endpoint       string
	Discovery      registry.Discovery
	TLS            bool
	TLSAuthType    int32
	Timeout        time.Duration
	KeepAlive      time.Duration
	MaxRetries     int
	RetryBackoff   time.Duration
	MaxConnections int
	// Middleware is reserved for future Kratos middleware; buildConnection uses gRPC interceptors only (tracing, retry, metrics, circuit breaker, logging).
	Middleware       []middleware.Middleware
	NodeFilter       selector.NodeFilter // Applied in LoadBalancer.SelectNode when configured
	Required         bool
	Metadata         map[string]string
	LoadBalancer     string
	CircuitBreaker   bool
	CircuitThreshold int
}

// NewGrpcClientPlugin creates a new gRPC client plugin instance
func NewGrpcClientPlugin() *ClientPlugin {
	metrics := NewClientMetrics()

	// Pool starts disabled; InitializeResources rebuilds it from config if pooling is on.
	connectionPool := NewConnectionPool(10, 5, 5*time.Minute, false, metrics)

	// Discovery is nil here and set per service later via SetDiscovery.
	loadBalancer := NewLoadBalancer(nil, metrics)

	circuitBreakers := NewCircuitBreakerManager(metrics)

	return &ClientPlugin{
		BasePlugin:      plugins.NewBasePlugin("grpc.client", "grpc.client", "gRPC client plugin for Lynx framework", "v1.5.5", "lynx.grpc.client", 20),
		conf:            &conf.GrpcClient{},
		connections:     make(map[string]*grpc.ClientConn),
		connectionPool:  connectionPool,
		loadBalancer:    loadBalancer,
		circuitBreakers: circuitBreakers,
		metrics:         metrics,
	}
}

// InitializeResources initializes the gRPC client plugin
func (c *ClientPlugin) InitializeResources(rt plugins.Runtime) error {
	if err := c.BasePlugin.InitializeResources(rt); err != nil {
		return err
	}
	// Store runtime for publishing readiness state
	c.rt = rt
	// Load configuration
	err := rt.GetConfig().Value("lynx.grpc.client").Scan(c.conf)
	if err != nil {
		return err
	}

	// Set default configuration
	if c.conf.DefaultTimeout == nil {
		c.conf.DefaultTimeout = &durationpb.Duration{Seconds: 10}
	}
	if c.conf.DefaultKeepAlive == nil {
		c.conf.DefaultKeepAlive = &durationpb.Duration{Seconds: 30}
	}
	if c.conf.MaxRetries == 0 {
		c.conf.MaxRetries = 3
	}
	if c.conf.RetryBackoff == nil {
		c.conf.RetryBackoff = &durationpb.Duration{Seconds: 1}
	}
	if c.conf.MaxConnections == 0 {
		c.conf.MaxConnections = 10
	}

	// Rebuild the pool from real config (the constructor created a disabled placeholder).
	// pool_size caps distinct services; max_connections caps connections per service.
	poolEnabled := c.conf.GetConnectionPooling()
	if poolEnabled {
		maxServices := int(c.conf.GetPoolSize())
		maxConnsPerService := int(c.conf.MaxConnections)
		idleTimeout := c.conf.GetIdleTimeout().AsDuration()
		if maxServices <= 0 {
			maxServices = 10
		}
		if maxConnsPerService <= 0 {
			maxConnsPerService = 5
		}
		if idleTimeout <= 0 {
			idleTimeout = 5 * time.Minute
		}
		c.connectionPool = NewConnectionPool(maxServices, maxConnsPerService, idleTimeout, poolEnabled, c.metrics)
	}

	c.discovery = c.resolveServiceDiscovery()

	// Validate configuration
	if err := c.validateConfiguration(); err != nil {
		return fmt.Errorf("configuration validation failed: %w", err)
	}

	// Initialize required-upstreams readiness as false until checks pass
	c.publishRequiredReadiness(false)

	return nil
}

func (c *ClientPlugin) resolveServiceDiscovery() registry.Discovery {
	if c == nil {
		return nil
	}
	if c.discovery != nil {
		return c.discovery
	}
	if c.rt != nil {
		for _, resourceName := range serviceDiscoverySharedResourceNames {
			resource, err := c.rt.GetSharedResource(resourceName)
			if err != nil || resource == nil {
				continue
			}
			discovery, ok := resource.(registry.Discovery)
			if ok {
				log.Infof("Resolved gRPC client service discovery from shared resource %s", resourceName)
				return discovery
			}
			log.Warnf("Shared resource %s does not implement registry.Discovery: %T", resourceName, resource)
		}
	}
	discovery, err := lynx.GetServiceDiscovery()
	if err == nil && discovery != nil {
		log.Infof("Resolved gRPC client service discovery from default Lynx app")
		return discovery
	}
	return nil
}

func (c *ClientPlugin) InitializeContext(ctx context.Context, plugin plugins.Plugin, rt plugins.Runtime) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context canceled before gRPC client initialize: %w", err)
	}
	return c.BasePlugin.Initialize(plugin, rt)
}

// StartupTasks starts the gRPC client plugin
func (c *ClientPlugin) StartupTasks() error {
	return c.startupWithContext(context.Background())
}

func (c *ClientPlugin) startupWithContext(ctx context.Context) error {
	log.Infof("Starting gRPC client plugin")
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context canceled before gRPC client startup: %w", err)
	}
	c.publishRuntimeContract(false, false)

	// Initialize metrics
	c.metrics.Initialize()

	// Initialize retry handler
	// c.retryHandler.Initialize(c.conf.MaxRetries, c.conf.RetryBackoff.AsDuration())

	// Ensure readiness is false until we complete checks
	c.publishRequiredReadiness(false)

	// Gate startup on required upstream readiness
	if err := c.CheckRequiredServicesContext(ctx); err != nil {
		log.Errorf("Required upstream services check failed: %v", err)
		return err
	}

	// Mark readiness true after required-check passes
	c.publishRequiredReadiness(true)

	if c.rt != nil {
		if err := c.rt.RegisterSharedResource(clientPluginName, c); err != nil {
			c.publishRuntimeContract(false, false)
			return fmt.Errorf("failed to register gRPC client shared resource: %w", err)
		}
		c.registerRuntimePluginAlias()
		if err := c.rt.RegisterPrivateResource("config", c.conf); err != nil {
			log.Warnf("failed to register gRPC client private config: %v", err)
		}
		if c.connectionPool != nil {
			if err := c.rt.RegisterPrivateResource("connection_pool", c.connectionPool); err != nil {
				log.Warnf("failed to register gRPC client private connection pool: %v", err)
			}
		}
		if c.loadBalancer != nil {
			if err := c.rt.RegisterPrivateResource("load_balancer", c.loadBalancer); err != nil {
				log.Warnf("failed to register gRPC client private load balancer: %v", err)
			}
		}
		if c.circuitBreakers != nil {
			if err := c.rt.RegisterPrivateResource("circuit_breakers", c.circuitBreakers); err != nil {
				log.Warnf("failed to register gRPC client private circuit breakers: %v", err)
			}
		}
		if c.discovery != nil {
			if err := c.rt.RegisterPrivateResource("discovery", c.discovery); err != nil {
				log.Warnf("failed to register gRPC client private discovery: %v", err)
			}
		}
		if c.metrics != nil {
			if err := c.rt.RegisterPrivateResource("metrics", c.metrics); err != nil {
				log.Warnf("failed to register gRPC client private metrics: %v", err)
			}
		}
		if len(c.connections) > 0 {
			if err := c.rt.RegisterPrivateResource("connections", c.connections); err != nil {
				log.Warnf("failed to register gRPC client private connections: %v", err)
			}
		}
		if c.tlsManager != nil {
			if err := c.rt.RegisterPrivateResource("tls_manager", c.tlsManager); err != nil {
				log.Warnf("failed to register gRPC client private TLS manager: %v", err)
			}
		}
	}

	if err := c.CheckHealth(); err != nil {
		c.publishRuntimeContract(false, false)
		return err
	}
	c.publishRuntimeContract(true, true)

	log.Infof("gRPC client plugin started successfully")
	return nil
}

func (c *ClientPlugin) StartContext(ctx context.Context, _ plugins.Plugin) error {
	return c.startupWithContext(ctx)
}

// CleanupTasks is called by the framework during plugin Stop to release all resources.
func (c *ClientPlugin) CleanupTasks() error {
	return c.CloseContext(context.Background())
}

func (c *ClientPlugin) StopContext(ctx context.Context, _ plugins.Plugin) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context canceled before gRPC client stop: %w", err)
	}
	return c.CloseContext(ctx)
}

// CloseServiceConnection closes connections for the given service (pool and legacy map).
// Use this when closing a single service so the next GetConnection creates a fresh connection.
func (c *ClientPlugin) CloseServiceConnection(serviceName string) error {
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		return fmt.Errorf("service name cannot be empty")
	}
	c.mu.Lock()
	delete(c.connections, serviceName)
	c.mu.Unlock()
	if c.connectionPool != nil {
		return c.connectionPool.CloseConnection(serviceName)
	}
	return nil
}

// Close closes all connections and cleans up resources
func (c *ClientPlugin) Close() error {
	return c.CloseContext(context.Background())
}

// CloseContext closes all connections and cleans up resources while honoring a caller-provided stop budget.
func (c *ClientPlugin) CloseContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context canceled before gRPC client close: %w", err)
	}
	c.publishRuntimeContract(false, false)

	c.mu.Lock()
	defer c.mu.Unlock()

	var lastErr error

	if c.connectionPool != nil {
		if err := c.connectionPool.CloseAll(); err != nil {
			lastErr = err
		}
	}

	if c.loadBalancer != nil {
		if err := c.loadBalancer.Close(); err != nil {
			lastErr = err
		}
	}

	if c.circuitBreakers != nil {
		c.circuitBreakers.Close()
	}

	if c.tlsManager != nil {
		c.tlsManager.Close()
	}

	for serviceName, conn := range c.connections {
		if err := conn.Close(); err != nil {
			lastErr = err
		}
		delete(c.connections, serviceName)
	}

	return lastErr
}

func (c *ClientPlugin) Configure(cfg any) error {
	if cfg == nil {
		return nil
	}
	grpcConf, ok := cfg.(*conf.GrpcClient)
	if !ok {
		return fmt.Errorf("invalid configuration type: expected *conf.GrpcClient, got %T", cfg)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	oldConf := c.conf
	c.conf = grpcConf
	c.discovery = c.resolveServiceDiscoveryLocked()
	if err := c.validateConfiguration(); err != nil {
		c.conf = oldConf
		return fmt.Errorf("gRPC client configuration validation failed: %w", err)
	}
	for serviceName, conn := range c.connections {
		if conn != nil {
			if err := conn.Close(); err != nil {
				log.Warnf("failed to close stale gRPC client connection for %s during reconfigure: %v", serviceName, err)
			}
		}
	}
	c.rebuildConnectionPoolLocked()
	c.connections = make(map[string]*grpc.ClientConn)
	if c.connectionPool != nil || len(c.connections) > 0 {
		log.Infof("gRPC client configuration updated; pooled connections were reset and new settings will apply on reconnect")
	}
	return nil
}

func (c *ClientPlugin) IsContextAware() bool {
	return true
}

// GetConnection returns a gRPC client connection for the specified service.
// It first tries subscribe_services config for that name; if not found, falls back to createConnection (global config + discovery).
func (c *ClientPlugin) GetConnection(serviceName string) (*grpc.ClientConn, error) {
	c.mu.RLock()
	conn, exists := c.connections[serviceName]
	c.mu.RUnlock()

	if exists && conn != nil {
		state := conn.GetState()
		if state == connectivity.Ready || state == connectivity.Idle {
			return conn, nil
		}
		c.mu.Lock()
		delete(c.connections, serviceName)
		c.mu.Unlock()
	}

	// Prefer subscribe_services config when the service is listed there.
	subConn, err := c.GetSubscribeServiceConnection(serviceName)
	if err == nil {
		return subConn, nil
	}
	// Fall back to legacy createConnection (global config + discovery).
	return c.createConnection(serviceName)
}

// CreateConnection creates a new gRPC connection based on the provided configuration
func (c *ClientPlugin) CreateConnection(config ClientConfig) (*grpc.ClientConn, error) {
	// Configure load balancer for this service if needed
	if config.Discovery != nil && config.LoadBalancer != "" {
		lbConfig := &LoadBalancerConfig{
			Strategy:   LoadBalancerType(config.LoadBalancer),
			Metadata:   config.Metadata,
			NodeFilter: config.NodeFilter,
		}
		c.loadBalancer.SetDiscovery(config.Discovery)
		if err := c.loadBalancer.ConfigureService(config.ServiceName, lbConfig); err != nil {
			log.Errorf("Failed to configure load balancer for service %s: %v", config.ServiceName, err)
		}
	}

	// Use connection pool to get/create connection (circuit breaker is applied per RPC in buildConnection interceptors).
	conn, err := c.connectionPool.GetConnection(config.ServiceName, func() (*grpc.ClientConn, error) {
		return c.buildConnection(config)
	})

	if err != nil {
		return nil, fmt.Errorf("failed to get connection for service %s: %w", config.ServiceName, err)
	}

	// Mirror into the legacy connections map so older GetConnection callers still find it.
	c.mu.Lock()
	c.connections[config.ServiceName] = conn
	c.mu.Unlock()

	if c.metrics != nil {
		c.metrics.RecordConnectionCreated(config.ServiceName)
	}

	return conn, nil
}

// createConnection creates a connection using default configuration
func (c *ClientPlugin) createConnection(serviceName string) (*grpc.ClientConn, error) {
	c.mu.RLock()
	discovery := c.discovery
	tlsEnable := false
	tlsAuthType := int32(0)
	maxRetries := int32(3)
	defaultTimeout := 10 * time.Second
	defaultKeepAlive := 30 * time.Second
	retryBackoff := time.Second
	maxConnections := int32(10)
	if c.conf != nil {
		tlsEnable = c.conf.GetTlsEnable()
		tlsAuthType = c.conf.GetTlsAuthType()
		if c.conf.MaxRetries > 0 {
			maxRetries = c.conf.MaxRetries
		}
		if c.conf.DefaultTimeout != nil {
			defaultTimeout = c.conf.DefaultTimeout.AsDuration()
		}
		if c.conf.DefaultKeepAlive != nil {
			defaultKeepAlive = c.conf.DefaultKeepAlive.AsDuration()
		}
		if c.conf.RetryBackoff != nil {
			retryBackoff = c.conf.RetryBackoff.AsDuration()
		}
		if c.conf.MaxConnections > 0 {
			maxConnections = c.conf.MaxConnections
		}
	}
	c.mu.RUnlock()

	config := ClientConfig{
		ServiceName:    serviceName,
		Discovery:      discovery,
		TLS:            tlsEnable,
		TLSAuthType:    tlsAuthType,
		MaxRetries:     int(maxRetries),
		Middleware:     c.getDefaultMiddleware(),
		Timeout:        defaultTimeout,
		KeepAlive:      defaultKeepAlive,
		RetryBackoff:   retryBackoff,
		MaxConnections: int(maxConnections),
	}

	return c.CreateConnection(config)
}

// normalizeGrpcTarget converts a registry endpoint (e.g. "grpc://host:port") to a gRPC dial target.
// Uses "passthrough:///" for direct connection to avoid extra DNS resolution.
func normalizeGrpcTarget(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	for _, prefix := range []string{"grpc://", "grpcs://", "http://", "https://"} {
		if strings.HasPrefix(addr, prefix) {
			addr = addr[len(prefix):]
			break
		}
	}
	if addr == "" {
		return ""
	}
	return "passthrough:///" + addr
}

// buildConnection builds a gRPC client connection with the given configuration
func (c *ClientPlugin) buildConnection(config ClientConfig) (*grpc.ClientConn, error) {
	return c.buildConnectionWithContext(context.Background(), config)
}

func (c *ClientPlugin) buildConnectionWithContext(ctx context.Context, config ClientConfig) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption

	// Pick the dial target: explicit LB picks one node, plain discovery dials all
	// instances (gRPC round_robin), otherwise a static endpoint.
	var target string
	if config.Discovery != nil && config.LoadBalancer != "" {
		// LB picks a single node, so each connection targets one address; the pool
		// holds several connections that may point at different nodes.
		lbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		node, _, err := c.loadBalancer.SelectNode(lbCtx, config.ServiceName)
		if err != nil {
			return nil, fmt.Errorf("load balancer select node for %s: %w", config.ServiceName, err)
		}
		target = normalizeGrpcTarget(node.Address())
		if target == "" {
			return nil, fmt.Errorf("empty address from node for service %s", config.ServiceName)
		}
	} else if config.Discovery != nil {
		opts = append(opts, grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`))
		target = fmt.Sprintf("discovery:///%s", config.ServiceName)
	} else if config.Endpoint != "" {
		opts = append(opts, grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`))
		target = config.Endpoint
	} else {
		return nil, fmt.Errorf("neither service discovery nor static endpoint configured for service %s", config.ServiceName)
	}

	// Add unary interceptors: tracing, retry, metrics, circuit breaker, logging.
	unaryChain := c.buildClientInterceptorChain(config)
	if len(unaryChain) > 0 {
		opts = append(opts, grpc.WithChainUnaryInterceptor(unaryChain...))
	}
	// Add stream interceptors: tracing, metrics, logging.
	streamChain := c.buildClientStreamInterceptorChain(config)
	if len(streamChain) > 0 {
		opts = append(opts, grpc.WithChainStreamInterceptor(streamChain...))
	}
	if config.NodeFilter != nil {
		// Node filter is applied via discovery/selector when using service discovery; no gRPC-level option.
	}

	if config.TLS {
		tlsConfig, err := c.buildTLSConfig(config)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS config: %w", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(tlsConfig))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	if config.KeepAlive > 0 {
		opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                config.KeepAlive,
			Timeout:             config.KeepAlive / 3,
			PermitWithoutStream: true,
		}))
	}

	// NewClient (not the deprecated DialContext) defers connecting until first use.
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, err
	}

	// Required services must be reachable at startup: force-connect and block until
	// Ready (or timeout) so a dead upstream fails startup instead of surfacing later.
	if config.Required {
		waitTimeout := config.Timeout
		if waitTimeout <= 0 {
			waitTimeout = 10 * time.Second
		}
		waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
		defer cancel()
		conn.Connect()
		for {
			state := conn.GetState()
			if state == connectivity.Ready {
				break
			}
			if !conn.WaitForStateChange(waitCtx, state) {
				_ = conn.Close()
				return nil, fmt.Errorf("connection to %s not ready within %v (last_state=%s)", target, waitTimeout, state.String())
			}
		}
	}

	return conn, nil
}

// buildTLSConfig builds TLS configuration for the client
func (c *ClientPlugin) buildTLSConfig(config ClientConfig) (credentials.TransportCredentials, error) {
	certProvider := c.getCertProvider()
	if certProvider == nil {
		return nil, fmt.Errorf("certificate provider not configured")
	}

	c.mu.Lock()
	if c.tlsManager == nil {
		c.tlsManager = NewTLSManager()
	}
	c.mu.Unlock()

	tlsConfig := &TLSConfig{
		Enabled:                  true,
		InsecureSkipVerify:       false,
		ServerName:               config.ServiceName,
		ClientAuth:               tls.ClientAuthType(config.TLSAuthType),
		MinVersion:               tls.VersionTLS12,
		MaxVersion:               tls.VersionTLS13,
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		},
	}

	// Inject Root CA from certificate provider for server cert verification (e.g. self-signed certs)
	if cp, ok := certProvider.(lynx.CertificateProvider); ok {
		if rootCA := cp.GetRootCACertificate(); len(rootCA) > 0 {
			tlsConfig.RootCACertPEM = rootCA
		}
	}

	err := c.tlsManager.SetServiceConfig(config.ServiceName, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to set TLS config for service %s: %w", config.ServiceName, err)
	}

	credList, err := c.tlsManager.GetCredentials(config.ServiceName)
	if err != nil {
		return nil, fmt.Errorf("failed to get TLS credentials for service %s: %w", config.ServiceName, err)
	}

	return credList, nil
}

// getDefaultMiddleware returns default middleware for gRPC clients
func (c *ClientPlugin) getDefaultMiddleware() []middleware.Middleware {
	return []middleware.Middleware{
		logging.Client(nil),
		tracing.Client(),
		c.getMetricsMiddleware(),
		// c.getRetryMiddleware(),
	}
}

// getMetricsMiddleware returns metrics middleware for gRPC clients.
// This is a Kratos-level middleware used for method-agnostic request tracking.
// Fine-grained per-method metrics are recorded by metricsUnaryClientInterceptor.
func (c *ClientPlugin) getMetricsMiddleware() middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			start := time.Now()

			resp, err := handler(ctx, req)

			duration := time.Since(start)
			s := "success"
			if err != nil {
				s = "error"
			}
			c.metrics.RecordRequest("unknown", "unknown", s, duration)

			return resp, err
		}
	}
}

// getRetryMiddleware returns a Kratos-level retry middleware for gRPC clients.
// Retries happen with exponential backoff, respecting context cancellation.
func (c *ClientPlugin) getRetryMiddleware() middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			maxRetries := 3
			baseDelay := 100 * time.Millisecond
			maxDelay := 5 * time.Second

			if c.conf != nil {
				if c.conf.MaxRetries > 0 {
					maxRetries = int(c.conf.MaxRetries)
				}
				if c.conf.RetryBackoff != nil {
					baseDelay = c.conf.RetryBackoff.AsDuration()
				}
			}

			var lastErr error
			for attempt := 0; attempt <= maxRetries; attempt++ {
				resp, err := handler(ctx, req)
				if err == nil {
					if attempt > 0 && c.metrics != nil {
						c.metrics.RecordRetry("unknown", "success", fmt.Sprintf("%d", attempt))
					}
					return resp, nil
				}

				lastErr = err
				if !c.isRetryableError(err) {
					if c.metrics != nil {
						c.metrics.RecordRetry("unknown", "non_retryable", fmt.Sprintf("%d", attempt))
					}
					return resp, err
				}
				if attempt == maxRetries {
					if c.metrics != nil {
						c.metrics.RecordRetry("unknown", "max_attempts", fmt.Sprintf("%d", attempt))
					}
					break
				}

				delay := c.calculateRetryDelay(attempt, baseDelay, maxDelay)
				retryTimer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					retryTimer.Stop()
					if c.metrics != nil {
						c.metrics.RecordRetry("unknown", "context_cancelled", fmt.Sprintf("%d", attempt))
					}
					return nil, ctx.Err()
				case <-retryTimer.C:
				}
			}

			return nil, lastErr
		}
	}
}

// GetConnectionCount returns the total number of active connections (legacy map + connection pool).
func (c *ClientPlugin) GetConnectionCount() int {
	c.mu.RLock()
	legacyCount := len(c.connections)
	c.mu.RUnlock()
	poolCount := 0
	if c.connectionPool != nil {
		poolCount = c.connectionPool.TotalConnectionCount()
	}
	// When pooling is enabled, connections are in the pool and also stored in c.connections for compatibility;
	// avoid double-counting by returning the larger of the two (typically pool has the real count).
	if poolCount > legacyCount {
		return poolCount
	}
	return legacyCount
}

// GetConnectionStatus returns the status of all connections (legacy map and pool services merged).
func (c *ClientPlugin) GetConnectionStatus() map[string]string {
	c.mu.RLock()
	s := make(map[string]string)
	for serviceName, conn := range c.connections {
		if conn != nil {
			s[serviceName] = conn.GetState().String()
		} else {
			s[serviceName] = "nil"
		}
	}
	c.mu.RUnlock()
	if c.connectionPool != nil {
		for name, status := range c.connectionPool.GetServiceStatus() {
			s[name] = status
		}
	}
	return s
}

// validateConfiguration validates the gRPC client configuration
func (c *ClientPlugin) validateConfiguration() error {
	if c.conf == nil {
		return fmt.Errorf("gRPC client configuration is nil")
	}

	// Validate subscribe services configuration
	for i, svc := range c.conf.SubscribeServices {
		if svc.Name == "" {
			return fmt.Errorf("subscribe service at index %d: service name is required", i)
		}

		// When using service discovery, endpoint should be empty or optional
		if c.discovery != nil && svc.Endpoint != "" {
			log.Warnf("Service %s has both service discovery and static endpoint configured. Service discovery will take precedence.", svc.Name)
		}

		// When no service discovery is available, endpoint is required (unless it's not required service)
		if c.discovery == nil && svc.Endpoint == "" && svc.Required {
			return fmt.Errorf("service %s is marked as required but has no endpoint and no service discovery available", svc.Name)
		}
	}

	// Validate legacy services configuration (deprecated)
	for i, svc := range c.conf.Services {
		if svc.Name == "" {
			return fmt.Errorf("legacy service at index %d: service name is required", i)
		}
		if svc.Endpoint == "" {
			return fmt.Errorf("legacy service %s: endpoint is required for static configuration", svc.Name)
		}
		log.Warnf("Using deprecated 'services' configuration for service %s. Please migrate to 'subscribe_services'.", svc.Name)
	}

	return nil
}

// SetDiscovery sets the service discovery instance
func (c *ClientPlugin) SetDiscovery(discovery registry.Discovery) {
	c.mu.Lock()
	c.discovery = discovery
	c.mu.Unlock()
	if c.rt != nil && discovery != nil {
		if err := c.rt.RegisterPrivateResource("discovery", discovery); err != nil {
			log.Warnf("failed to update gRPC client private discovery resource: %v", err)
		}
	}
	log.Infof("Service discovery set for gRPC client plugin")
}

func (c *ClientPlugin) PluginProtocol() plugins.PluginProtocol {
	protocol := c.BasePlugin.PluginProtocol()
	protocol.ContextLifecycle = true
	return protocol
}

// buildSubscribeServiceConfig builds ClientConfig for a subscribe service by name (for use by GetSubscribeServiceConnection and CheckRequiredServices).
func (c *ClientPlugin) buildSubscribeServiceConfig(serviceName string) (ClientConfig, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var serviceConfig *conf.SubscribeService
	if c.conf == nil {
		return ClientConfig{}, fmt.Errorf("gRPC client configuration is nil")
	}
	for _, svc := range c.conf.SubscribeServices {
		if svc.Name == serviceName {
			serviceConfig = svc
			break
		}
	}
	if serviceConfig == nil {
		return ClientConfig{}, fmt.Errorf("service %s not found in subscribe services configuration", serviceName)
	}

	config := ClientConfig{
		ServiceName:      serviceConfig.Name,
		Discovery:        c.discovery,
		TLS:              serviceConfig.TlsEnable,
		TLSAuthType:      serviceConfig.TlsAuthType,
		MaxRetries:       int(serviceConfig.MaxRetries),
		Required:         serviceConfig.Required,
		Metadata:         serviceConfig.Metadata,
		LoadBalancer:     serviceConfig.LoadBalancer,
		CircuitBreaker:   serviceConfig.CircuitBreakerEnabled,
		CircuitThreshold: int(serviceConfig.CircuitBreakerThreshold),
	}

	if serviceConfig.Timeout != nil {
		config.Timeout = serviceConfig.Timeout.AsDuration()
	} else if c.conf.DefaultTimeout != nil {
		config.Timeout = c.conf.DefaultTimeout.AsDuration()
	} else {
		config.Timeout = 10 * time.Second
	}
	if c.discovery == nil && serviceConfig.Endpoint != "" {
		config.Endpoint = serviceConfig.Endpoint
		log.Infof("Using static endpoint for service %s: %s", serviceName, serviceConfig.Endpoint)
	} else if c.discovery != nil {
		log.Infof("Using service discovery for service %s", serviceName)
	} else if serviceConfig.Required {
		return ClientConfig{}, fmt.Errorf("service %s is required but has no endpoint and no service discovery available", serviceName)
	}
	if c.conf.DefaultKeepAlive != nil {
		config.KeepAlive = c.conf.DefaultKeepAlive.AsDuration()
	} else {
		config.KeepAlive = 30 * time.Second
	}
	if c.conf.RetryBackoff != nil {
		config.RetryBackoff = c.conf.RetryBackoff.AsDuration()
	} else {
		config.RetryBackoff = 1 * time.Second
	}
	if c.conf.MaxConnections > 0 {
		config.MaxConnections = int(c.conf.MaxConnections)
	} else {
		config.MaxConnections = 10
	}
	config.Middleware = c.getDefaultMiddleware()
	return config, nil
}

// GetSubscribeServiceConnection creates a connection for a subscribe service (uses pool when enabled).
func (c *ClientPlugin) GetSubscribeServiceConnection(serviceName string) (*grpc.ClientConn, error) {
	config, err := c.buildSubscribeServiceConfig(serviceName)
	if err != nil {
		return nil, err
	}
	return c.CreateConnection(config)
}

// CheckRequiredServices checks if all required services are available at startup.
// Uses a temporary connection (buildConnection only, not pooled) so that closing it does not corrupt the connection pool.
func (c *ClientPlugin) CheckRequiredServices() error {
	return c.CheckRequiredServicesContext(context.Background())
}

func (c *ClientPlugin) CheckRequiredServicesContext(ctx context.Context) error {
	c.mu.RLock()
	var services []*conf.SubscribeService
	if c.conf != nil {
		services = append(services, c.conf.SubscribeServices...)
	}
	c.mu.RUnlock()

	required := make([]*conf.SubscribeService, 0, len(services))
	for _, svc := range services {
		if svc.Required {
			required = append(required, svc)
		}
	}
	if len(required) == 0 {
		return nil
	}

	const maxConcurrentRequiredChecks = 8
	limit := maxConcurrentRequiredChecks
	if len(required) < limit {
		limit = len(required)
	}

	checkCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan *conf.SubscribeService)
	errCh := make(chan error, len(required))
	var wg sync.WaitGroup

	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for svc := range jobs {
				if err := c.checkRequiredService(checkCtx, svc); err != nil {
					errCh <- err
					cancel()
				}
			}
		}()
	}

dispatch:
	for _, svc := range required {
		select {
		case <-checkCtx.Done():
			break dispatch
		case jobs <- svc:
		}
	}
	close(jobs)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("gRPC client startup canceled while checking required services: %w", err)
	}
	return nil
}

func (c *ClientPlugin) checkRequiredService(ctx context.Context, svc *conf.SubscribeService) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("gRPC client startup canceled while checking required services: %w", err)
	}

	log.Infof("Checking required service: %s", svc.Name)

	config, err := c.buildSubscribeServiceConfig(svc.Name)
	if err != nil {
		return fmt.Errorf("required service %s config: %w", svc.Name, err)
	}

	// Use buildConnection only (no pool) so closing does not leave a closed conn in the pool.
	conn, err := c.buildConnectionWithContext(ctx, config)
	if err != nil {
		return fmt.Errorf("required service %s is not available: %w", svc.Name, err)
	}
	if conn != nil {
		if err := conn.Close(); err != nil {
			log.Error(err)
			return err
		}
	}

	log.Infof("Required service %s is available", svc.Name)
	return nil
}

// isRetryableError reports whether the interceptor should retry err. Only
// transient/server-side codes are retried; client-fault codes cannot succeed on
// retry and are returned immediately. This path is conservative: anything not a
// known-retryable gRPC code (including context cancellation/deadline and non-gRPC
// errors) is treated as non-retryable, unlike RetryHandler.isRetryableError.
func (c *ClientPlugin) isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unavailable,
			codes.DeadlineExceeded,
			codes.ResourceExhausted,
			codes.Aborted,
			codes.Internal:
			return true
		case codes.InvalidArgument,
			codes.NotFound,
			codes.AlreadyExists,
			codes.PermissionDenied,
			codes.Unauthenticated,
			codes.FailedPrecondition,
			codes.OutOfRange,
			codes.Unimplemented:
			return false
		default:
			return false
		}
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}

	return false
}

// calculateRetryDelay returns baseDelay*2^attempt capped at maxDelay, then adds
// ±25% jitter so retrying clients don't synchronize into a thundering herd.
// A negative result (possible at the jitter floor) falls back to baseDelay.
func (c *ClientPlugin) calculateRetryDelay(attempt int, baseDelay, maxDelay time.Duration) time.Duration {
	delay := time.Duration(float64(baseDelay) * math.Pow(2, float64(attempt)))
	if delay > maxDelay {
		delay = maxDelay
	}

	jitter := time.Duration(float64(delay) * 0.25 * (rand.Float64()*2 - 1))
	delay += jitter

	if delay < 0 {
		delay = baseDelay
	}

	return delay
}

// getCertProvider gets the certificate provider from the application.
func (c *ClientPlugin) getCertProvider() any {
	if lynx.Lynx() == nil {
		return nil
	}
	return lynx.Lynx().Certificate()
}
