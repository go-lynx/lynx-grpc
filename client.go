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

	// Initialize connection pool with default settings
	// maxServices: 10, maxConnsPerService: 5, idleTimeout: 5min, enabled: false
	connectionPool := NewConnectionPool(10, 5, 5*time.Minute, false, metrics)

	// Initialize load balancer (will be configured per service)
	loadBalancer := NewLoadBalancer(nil, metrics)

	// Initialize circuit breaker manager
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

	// Initialize connection pool with actual config
	poolEnabled := c.conf.GetConnectionPooling()
	if poolEnabled {
		maxServices := int(c.conf.GetPoolSize())         // Total number of services
		maxConnsPerService := int(c.conf.MaxConnections) // Connections per service
		idleTimeout := c.conf.GetIdleTimeout().AsDuration()
		if maxServices <= 0 {
			maxServices = 10
		}
		if maxConnsPerService <= 0 {
			maxConnsPerService = 5 // Default: 5 connections per service
		}
		if idleTimeout <= 0 {
			idleTimeout = 5 * time.Minute
		}
		// Recreate connection pool with actual config
		// Now supports multiple connections per service (channel pool)
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
			return fmt.Errorf("failed to register gRPC client shared resource: %w", err)
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

	c.mu.Lock()
	defer c.mu.Unlock()

	var lastErr error

	// Close connection pool
	if c.connectionPool != nil {
		if err := c.connectionPool.CloseAll(); err != nil {
			lastErr = err
		}
	}

	// Close load balancer
	if c.loadBalancer != nil {
		if err := c.loadBalancer.Close(); err != nil {
			lastErr = err
		}
	}

	// Close circuit breakers
	if c.circuitBreakers != nil {
		c.circuitBreakers.Close()
	}

	// Close TLS manager
	if c.tlsManager != nil {
		c.tlsManager.Close()
	}

	// Close legacy connections
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

	// Store connection in legacy map for backward compatibility
	c.mu.Lock()
	c.connections[config.ServiceName] = conn
	c.mu.Unlock()

	// Record metrics
	if c.metrics != nil {
		c.metrics.RecordConnectionCreated(config.ServiceName)
	}

	return conn, nil
}

// createConnection creates a connection using default configuration
func (c *ClientPlugin) createConnection(serviceName string) (*grpc.ClientConn, error) {
	config := ClientConfig{
		ServiceName: serviceName,
		Discovery:   c.discovery,
		TLS:         c.conf.GetTlsEnable(),
		TLSAuthType: c.conf.GetTlsAuthType(),
		MaxRetries:  int(c.conf.MaxRetries),
		Middleware:  c.getDefaultMiddleware(),
	}

	// Set timeout with nil check
	if c.conf.DefaultTimeout != nil {
		config.Timeout = c.conf.DefaultTimeout.AsDuration()
	} else {
		config.Timeout = 10 * time.Second
	}

	// Set keep alive with nil check
	if c.conf.DefaultKeepAlive != nil {
		config.KeepAlive = c.conf.DefaultKeepAlive.AsDuration()
	} else {
		config.KeepAlive = 30 * time.Second
	}

	// Set retry backoff with nil check
	if c.conf.RetryBackoff != nil {
		config.RetryBackoff = c.conf.RetryBackoff.AsDuration()
	} else {
		config.RetryBackoff = 1 * time.Second
	}

	// Set max connections
	if c.conf.MaxConnections > 0 {
		config.MaxConnections = int(c.conf.MaxConnections)
	} else {
		config.MaxConnections = 10
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
	// Build client options
	var opts []grpc.DialOption

	// Set endpoint based on configuration: LoadBalancer path (select one node) or discovery/static
	var target string
	if config.Discovery != nil && config.LoadBalancer != "" {
		// Use LoadBalancer to select one node and dial it (connection-level load balancing)
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
		// Single address per connection; pool may have multiple connections to different nodes
	} else if config.Discovery != nil {
		// Use service discovery (resolver returns all instances, gRPC round_robin)
		opts = append(opts, grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`))
		target = fmt.Sprintf("discovery:///%s", config.ServiceName)
	} else if config.Endpoint != "" {
		// Use static endpoint
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

	// Add TLS configuration if enabled
	if config.TLS {
		tlsConfig, err := c.buildTLSConfig(config)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS config: %w", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(tlsConfig))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Add keep-alive configuration
	if config.KeepAlive > 0 {
		opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                config.KeepAlive,
			Timeout:             config.KeepAlive / 3,
			PermitWithoutStream: true,
		}))
	}

	// Create connection using NewClient (DialContext is deprecated in newer gRPC)
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, err
	}

	// If the service is required, block until the connection is Ready or timeout
	if config.Required {
		waitTimeout := config.Timeout
		if waitTimeout <= 0 {
			waitTimeout = 10 * time.Second
		}
		waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
		defer cancel()
		// Start connecting and wait for Ready
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
	// Get certificate provider from the application
	certProvider := c.getCertProvider()
	if certProvider == nil {
		return nil, fmt.Errorf("certificate provider not configured")
	}

	// Create TLS manager if not exists (concurrency-safe)
	c.mu.Lock()
	if c.tlsManager == nil {
		c.tlsManager = NewTLSManager()
	}
	c.mu.Unlock()

	// Build TLS configuration based on auth type
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

	// Set service-specific TLS configuration
	err := c.tlsManager.SetServiceConfig(config.ServiceName, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to set TLS config for service %s: %w", config.ServiceName, err)
	}

	// Get credentials from TLS manager
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

// getMetricsMiddleware returns metrics middleware for gRPC clients
func (c *ClientPlugin) getMetricsMiddleware() middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req interface{}) (interface{}, error) {
			start := time.Now()

			resp, err := handler(ctx, req)

			duration := time.Since(start)
			s := "success"
			if err != nil {
				s = "error"
			}

			// Record metrics
			c.metrics.RecordRequest("unknown", "unknown", s, duration)

			return resp, err
		}
	}
}

// getRetryMiddleware returns retry middleware for gRPC clients
func (c *ClientPlugin) getRetryMiddleware() middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req interface{}) (interface{}, error) {
			// Get retry configuration from context or use defaults
			maxRetries := 3
			baseDelay := 100 * time.Millisecond
			maxDelay := 5 * time.Second

			// Try to get retry config from client configuration
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
				// First attempt or retry
				resp, err := handler(ctx, req)

				// If successful, return immediately
				if err == nil {
					if attempt > 0 {
						// Record retry success metrics
						if c.metrics != nil {
							c.metrics.RecordRetry("unknown", "success", fmt.Sprintf("%d", attempt))
						}
					}
					return resp, nil
				}

				lastErr = err

				// Check if error is retryable
				if !c.isRetryableError(err) {
					// Non-retryable error, return immediately
					if c.metrics != nil {
						c.metrics.RecordRetry("unknown", "non_retryable", fmt.Sprintf("%d", attempt))
					}
					return resp, err
				}

				// If this was the last attempt, don't wait
				if attempt == maxRetries {
					if c.metrics != nil {
						c.metrics.RecordRetry("unknown", "max_attempts", fmt.Sprintf("%d", attempt))
					}
					break
				}

				// Calculate delay with exponential backoff
				delay := c.calculateRetryDelay(attempt, baseDelay, maxDelay)

				// Wait before retry, but respect context cancellation
				retryTimer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					retryTimer.Stop()
					if c.metrics != nil {
						c.metrics.RecordRetry("unknown", "context_cancelled", fmt.Sprintf("%d", attempt))
					}
					return nil, ctx.Err()
				case <-retryTimer.C:
					// Continue to next retry
				}
			}

			// All retries exhausted, return last error
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
	c.discovery = discovery
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
	var serviceConfig *conf.SubscribeService
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
	for _, svc := range c.conf.SubscribeServices {
		if !svc.Required {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("gRPC client startup canceled while checking required services: %w", err)
		}

		log.Infof("Checking required service: %s", svc.Name)

		config, err := c.buildSubscribeServiceConfig(svc.Name)
		if err != nil {
			return fmt.Errorf("required service %s config: %w", svc.Name, err)
		}

		// Use buildConnection only (no pool) so closing does not leave a closed conn in the pool
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
	}

	return nil
}

// isRetryableError determines if an error is retryable
func (c *ClientPlugin) isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// Check for gRPC status codes
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

	// Check for context errors
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false // Don't retry context errors
	}

	// Default to not retryable for unknown errors
	return false
}

// calculateRetryDelay calculates the delay for the next retry attempt using exponential backoff with jitter
func (c *ClientPlugin) calculateRetryDelay(attempt int, baseDelay, maxDelay time.Duration) time.Duration {
	// Exponential backoff: baseDelay * 2^attempt
	delay := time.Duration(float64(baseDelay) * math.Pow(2, float64(attempt)))

	// Cap at maxDelay
	if delay > maxDelay {
		delay = maxDelay
	}

	// Add jitter to avoid thundering herd (±25% random variation)
	jitter := time.Duration(float64(delay) * 0.25 * (rand.Float64()*2 - 1))
	delay += jitter

	// Ensure delay is not negative
	if delay < 0 {
		delay = baseDelay
	}

	return delay
}

// getCertProvider gets the certificate provider from the application
func (c *ClientPlugin) getCertProvider() interface{} {
	// Get certificate provider from the Lynx application
	if lynx.Lynx() == nil {
		return nil
	}
	return lynx.Lynx().Certificate()
}
