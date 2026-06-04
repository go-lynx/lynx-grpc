// Package grpc provides a gRPC service plugin for the Lynx framework.
// It implements the necessary interfaces to integrate with the Lynx plugin system
// and provides functionality for setting up and managing a gRPC service with various
// middleware options and TLS support.
package grpc

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/go-kratos/kratos/contrib/middleware/validate/v2"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/middleware/tracing"
	"github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/go-lynx/lynx"
	"github.com/go-lynx/lynx-grpc/conf"
	"github.com/go-lynx/lynx/plugins"
	grpcgo "google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	pluginName        = "grpc.service"
	pluginVersion     = "v1.6.1"
	pluginDescription = "grpc service plugin for lynx framework"
	// confPrefix is the config key prefix; both conf.Service and ServerOptions load from it.
	confPrefix = "lynx.grpc.service"
)

// Service is the Lynx gRPC server plugin: wraps a Kratos gRPC server with
// lifecycle management, health reporting, metrics, and TLS.
type Service struct {
	*plugins.BasePlugin
	server      *grpc.Server
	// healthServer reports readiness via the gRPC health protocol.
	healthServer *health.Server
	// cancel function for background health status poller
	healthPollCancel context.CancelFunc
	// wait for the health poller goroutine to exit before returning from cleanup
	healthPollWG sync.WaitGroup
	conf *conf.Service
	rt   plugins.Runtime
	// Dependency injection providers; any nil provider falls back to the global Lynx app.
	appNameProvider      func() string
	loggerProvider       func() any
	certProvider         func() any
	controlPlaneProvider func() any
	// Port availability check cache to avoid hammering local ports from health probes.
	// Failures and successes are cached briefly.
	portCheckCache struct {
		mu            sync.RWMutex
		lastFailure   time.Time
		lastSuccess   time.Time
		lastError     error
		retryWindow   time.Duration // Avoid retrying failed checks within this window
		successWindow time.Duration
	}
	// Track whether we've already logged the "resource not found" message
	readinessResourceLogged struct {
		mu    sync.RWMutex
		value bool
	}
	// serverOpts holds optional server options (shutdown timeout, toggles, rate limit, inflight). Set in InitializeResources.
	serverOpts *serverOptsWithLimiter
	// confMu protects conf pointer replacement in Configure and health reads.
	confMu sync.RWMutex
}

// healthStatusPoller periodically syncs health serving status with required upstream readiness.
// It runs until the context is canceled in CleanupTasks.
func (g *Service) healthStatusPoller(ctx context.Context) {
	// Poll at a low frequency to avoid overhead, yet responsive enough for rollouts
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			updateCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
			g.updateHealthServingStatusWithContext(updateCtx)
			cancel()
		}
	}
}

// updateHealthServingStatusWithContext updates health status with context support.
// updateHealthServingStatus performs only in-memory mutex operations and map lookups
// (no I/O, no blocking calls), so it completes in microseconds. A simple context
// pre-check is sufficient; no goroutine/timeout wrapper is needed.
func (g *Service) updateHealthServingStatusWithContext(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	default:
		g.updateHealthServingStatus()
	}
}

// NewGrpcService returns an unstarted gRPC service plugin; config is loaded later
// in InitializeResources.
func NewGrpcService() *Service {
	return &Service{
		BasePlugin: plugins.NewBasePlugin(
			plugins.GeneratePluginID("", pluginName, pluginVersion),
			pluginName,
			pluginDescription,
			pluginVersion,
			confPrefix,
			10,
		),
		// Cache windows for port health checks: brief so failures clear fast,
		// long enough to avoid hammering the listener on every probe.
		portCheckCache: struct {
			mu            sync.RWMutex
			lastFailure   time.Time
			lastSuccess   time.Time
			lastError     error
			retryWindow   time.Duration
			successWindow time.Duration
		}{
			retryWindow:   500 * time.Millisecond,
			successWindow: time.Second,
		},
	}
}

// SetDependencies sets the dependency injection providers for the gRPC service.
// All provider functions may be nil, in which case the plugin falls back to the global Lynx app instance.
func (g *Service) SetDependencies(
	appNameProvider func() string,
	loggerProvider func() any,
	certProvider func() any,
	controlPlaneProvider func() any,
) {
	g.appNameProvider = appNameProvider
	g.loggerProvider = loggerProvider
	g.certProvider = certProvider
	g.controlPlaneProvider = controlPlaneProvider
}

// InitializeResources loads, defaults, and validates the gRPC server config and
// server options from the runtime; it does not start the server (see StartupTasks).
func (g *Service) InitializeResources(rt plugins.Runtime) error {
	if err := g.BasePlugin.InitializeResources(rt); err != nil {
		return err
	}
	g.rt = rt
	g.conf = &conf.Service{}

	err := rt.GetConfig().Value(confPrefix).Scan(g.conf)
	if err != nil {
		return err
	}

	// Defaults applied to any unset field below: TCP on :9090, TLS off, 10s timeout.
	defaultConf := &conf.Service{
		Network:     "tcp",
		Addr:        ":9090",
		TlsEnable:   false,
		TlsAuthType: 0,
		Timeout:     &durationpb.Duration{Seconds: 10},
	}

	if g.conf.Network == "" {
		g.conf.Network = defaultConf.Network
	}
	if g.conf.Addr == "" {
		g.conf.Addr = defaultConf.Addr
	}
	if g.conf.Timeout == nil {
		g.conf.Timeout = defaultConf.Timeout
	}

	if err := g.validateConfig(); err != nil {
		return fmt.Errorf("invalid gRPC configuration: %v", err)
	}

	// Server options share the config prefix so YAML can set them without proto changes.
	opts := defaultServerOptions()
	_ = rt.GetConfig().Value(confPrefix).Scan(&opts)
	g.serverOpts = buildServerOptionsFromConfig(g.conf, opts)

	return nil
}

func (g *Service) InitializeContext(ctx context.Context, plugin plugins.Plugin, rt plugins.Runtime) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context canceled before gRPC service initialize: %w", err)
	}
	return g.BasePlugin.Initialize(plugin, rt)
}

// StartupTasks builds and starts the gRPC server with middleware for tracing,
// logging, rate limiting, validation, and panic recovery.
func (g *Service) StartupTasks() error {
	return g.startupWithContext(context.Background())
}

func (g *Service) startupWithContext(ctx context.Context) error {
	log.Info("starting grpc service")
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context canceled before gRPC service startup: %w", err)
	}

	var middlewares []middleware.Middleware

	// Tracing: only when enabled in server options (default true).
	enableTracing := true
	if g.serverOpts != nil {
		enableTracing = g.serverOpts.opts.EnableTracing
	}
	if enableTracing {
		middlewares = append(middlewares, tracing.Server(tracing.WithTracerName(g.getAppName())))
	}
	middlewares = append(middlewares,
		validate.ProtoValidate(),
		recovery.Recovery(
			recovery.WithHandler(func(ctx context.Context, req, err any) error {
				log.Context(ctx).Error("panic recovery", "error", err)
				g.recordServerError("panic_recovery")
				return nil
			}),
		),
	)
	if rl := g.getGRPCRateLimit(); rl != nil {
		middlewares = append(middlewares, rl)
		log.Info("Added control-plane gRPC rate limiting middleware")
	}
	gMiddlewares := grpc.Middleware(middlewares...)

	// Native gRPC interceptors: unary (rate limit, inflight, metrics, logging) and stream (metrics, logging).
	serverUnaryInterceptors := g.getServerInterceptorChain()
	serverStreamInterceptors := g.getServerStreamInterceptorChain()
	opts := []grpc.ServerOption{
		grpc.Options(
			grpcgo.ChainUnaryInterceptor(serverUnaryInterceptors...),
			grpcgo.ChainStreamInterceptor(serverStreamInterceptors...),
		),
		gMiddlewares,
		grpc.CustomHealth(),
	}

	if g.conf.Network != "" {
		opts = append(opts, grpc.Network(g.conf.Network))
	}
	if g.conf.Addr != "" {
		opts = append(opts, grpc.Address(g.conf.Addr))
	}
	if g.conf.Timeout != nil {
		opts = append(opts, grpc.Timeout(g.conf.Timeout.AsDuration()))
	}
	if g.conf.GetTlsEnable() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("gRPC service startup canceled before TLS initialization: %w", err)
		}
		tlsOption, err := g.tlsLoad()
		if err != nil {
			return fmt.Errorf("failed to load TLS configuration: %v", err)
		}
		opts = append(opts, tlsOption)
	}

	// Cap concurrent streams per connection to bound server resource use. Unlike raw
	// gRPC (where 0 = unlimited), an unset value here gets a safe 1000 default.
	maxStreams := g.conf.GetMaxConcurrentStreams()
	if maxStreams > 0 {
		log.Infof("Setting gRPC MaxConcurrentStreams to %d", maxStreams)
		opts = append(opts, grpc.Options(grpcgo.MaxConcurrentStreams(uint32(maxStreams))))
	} else {
		const defaultMaxStreams = uint32(1000)
		log.Infof("MaxConcurrentStreams not configured, using safe default: %d", defaultMaxStreams)
		opts = append(opts, grpc.Options(grpcgo.MaxConcurrentStreams(defaultMaxStreams)))
	}

	// Configure message size limits (defaults to 10MB if not specified)
	const defaultMaxMsgSize = 10 * 1024 * 1024

	maxRecv := g.conf.GetMaxRecvMsgSize()
	switch {
	case maxRecv > 0:
		log.Infof("Setting gRPC MaxRecvMsgSize to %d bytes", maxRecv)
		opts = append(opts, grpc.Options(grpcgo.MaxRecvMsgSize(int(maxRecv))))
	default:
		log.Infof("MaxRecvMsgSize not configured, using safe default: %d bytes", defaultMaxMsgSize)
		opts = append(opts, grpc.Options(grpcgo.MaxRecvMsgSize(defaultMaxMsgSize)))
	}

	maxSend := g.conf.GetMaxSendMsgSize()
	switch {
	case maxSend > 0:
		log.Infof("Setting gRPC MaxSendMsgSize to %d bytes", maxSend)
		opts = append(opts, grpc.Options(grpcgo.MaxSendMsgSize(int(maxSend))))
	default:
		log.Infof("MaxSendMsgSize not configured, using safe default: %d bytes", defaultMaxMsgSize)
		opts = append(opts, grpc.Options(grpcgo.MaxSendMsgSize(defaultMaxMsgSize)))
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("gRPC service startup canceled before server creation: %w", err)
	}
	g.publishRuntimeContract(false, false)
	g.server = grpc.NewServer(opts...)

	// Serve the standard gRPC health service; initial status reflects required-upstream readiness.
	g.healthServer = health.NewServer()
	grpc_health_v1.RegisterHealthServer(g.server.Server, g.healthServer)
	g.updateHealthServingStatus()

	// cleanup runs on any early return below; cleared to nil on success.
	var cleanup func()
	defer func() {
		if cleanup != nil {
			log.Warnf("Startup failed, cleaning up resources")
			cleanup()
		}
	}()

	// Start background poller to keep serving status in sync with required upstream readiness
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("gRPC service startup canceled before health poller startup: %w", err)
	}
	pollCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	g.healthPollCancel = cancel
	g.healthPollWG.Add(1)
	go func() {
		defer g.healthPollWG.Done()
		g.healthStatusPoller(pollCtx)
	}()
	cleanup = func() {
		cancel()
		g.healthPollWG.Wait()
	}

	g.recordServerStartTime()

	if g.rt != nil {
		if err := g.rt.RegisterSharedResource(pluginName, g); err != nil {
			g.publishRuntimeContract(false, false)
			return fmt.Errorf("failed to register gRPC service shared resource: %w", err)
		}
		g.registerRuntimePluginAlias()
		if err := g.rt.RegisterPrivateResource("config", g.conf); err != nil {
			log.Warnf("failed to register gRPC service private config resource: %v", err)
		}
		if g.server != nil {
			if err := g.rt.RegisterPrivateResource("server", g.server); err != nil {
				log.Warnf("failed to register gRPC service private server resource: %v", err)
			}
		}
		if g.healthServer != nil {
			if err := g.rt.RegisterPrivateResource("health_server", g.healthServer); err != nil {
				log.Warnf("failed to register gRPC service private health server resource: %v", err)
			}
		}
		if g.serverOpts != nil {
			if err := g.rt.RegisterPrivateResource("server_options", g.serverOpts); err != nil {
				log.Warnf("failed to register gRPC service private server options resource: %v", err)
			}
		}
	}

	cleanup = nil
	g.publishRuntimeContract(true, true)

	log.Info("grpc service successfully started")
	return nil
}

func (g *Service) StartContext(ctx context.Context, _ plugins.Plugin) error {
	return g.startupWithContext(ctx)
}

// CleanupTasks gracefully stops the gRPC server.
// Timeout is read from graceful_shutdown_timeout config, defaulting to 30 s.
func (g *Service) CleanupTasks() error {
	timeout := 30 * time.Second
	if g.serverOpts != nil {
		timeout = g.serverOpts.opts.GracefulShutdownTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return g.CleanupTasksContext(ctx)
}

// CleanupTasksContext implements context-aware cleanup with proper timeout handling.
func (g *Service) CleanupTasksContext(parentCtx context.Context) error {
	if g.server == nil {
		return nil
	}
	g.publishRuntimeContract(false, false)

	var ctx context.Context
	var cancel context.CancelFunc
	if _, ok := parentCtx.Deadline(); ok {
		ctx = parentCtx
		cancel = func() {}
	} else {
		timeout := 30 * time.Second
		if g.serverOpts != nil && g.serverOpts.opts.GracefulShutdownTimeout > 0 {
			timeout = g.serverOpts.opts.GracefulShutdownTimeout
		} else if g.conf != nil && g.conf.GetTimeout() != nil && g.conf.GetTimeout().AsDuration() > 0 {
			timeout = g.conf.GetTimeout().AsDuration()
		}
		ctx, cancel = context.WithTimeout(parentCtx, timeout)
	}
	defer cancel()

	// Stop background poller and shutdown health server
	if err := g.stopHealthPoller(ctx); err != nil {
		return plugins.NewPluginError(g.ID(), "Stop", "Failed to stop gRPC health poller", err)
	}
	if g.healthServer != nil {
		g.healthServer.Shutdown()
	}

	if err := g.server.Stop(ctx); err != nil {
		// If stopping fails, return plugin error with error information
		return plugins.NewPluginError(g.ID(), "Stop", "Failed to stop gRPC server", err)
	}
	return nil
}

func (g *Service) StopContext(ctx context.Context, _ plugins.Plugin) error {
	return g.CleanupTasksContext(ctx)
}

func (g *Service) stopHealthPoller(ctx context.Context) error {
	if g.healthPollCancel == nil {
		return nil
	}

	g.healthPollCancel()
	done := make(chan struct{})
	go func() {
		g.healthPollWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		g.healthPollCancel = nil
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Configure allows runtime configuration updates for the gRPC server.
// It accepts an any parameter that should contain the new configuration
// and updates the server settings accordingly.
// Configure is concurrency-safe: the conf pointer swap is protected by confMu.
func (g *Service) Configure(c any) error {
	if c == nil {
		return fmt.Errorf("configuration cannot be nil")
	}

	// Try to convert the incoming configuration to *conf.Service type
	grpcConf, ok := c.(*conf.Service)
	if !ok {
		return fmt.Errorf("invalid configuration type: expected *conf.Service, got %T", c)
	}

	g.confMu.Lock()
	defer g.confMu.Unlock()

	oldConf := g.conf
	g.conf = grpcConf

	// Validate the new configuration
	if err := g.validateConfig(); err != nil {
		g.conf = oldConf
		log.Errorf("Invalid new gRPC configuration, rolling back: %v", err)
		return fmt.Errorf("gRPC configuration validation failed: %w", err)
	}

	g.rebuildServerOptions()

	log.Infof("gRPC configuration updated successfully")
	if g.server != nil {
		log.Infof("gRPC service dynamic options were refreshed; listener and interceptor chain changes still require managed restart")
	}
	return nil
}

func (g *Service) PluginProtocol() plugins.PluginProtocol {
	protocol := g.BasePlugin.PluginProtocol()
	protocol.ContextLifecycle = true
	return protocol
}

func (g *Service) IsContextAware() bool {
	return true
}

// CheckHealth implements the health check interface for the gRPC server.
// It performs necessary health checks and updates the provided health report
// with the current status of the server.
func (g *Service) CheckHealth() error {
	if g.server == nil {
		return fmt.Errorf("gRPC server is not initialized")
	}
	g.confMu.RLock()
	defer g.confMu.RUnlock()

	if g.conf == nil || g.conf.Addr == "" {
		return fmt.Errorf("gRPC server address not configured")
	}

	// A live server must be accepting connections; an unreachable port is unhealthy.
	if err := g.checkPortAvailability(); err != nil {
		g.recordHealthCheckMetricsInternal(false)
		return fmt.Errorf("gRPC server is not listening on %s: %w", g.conf.Addr, err)
	}

	if g.conf.GetTlsEnable() {
		if err := g.validateTLSConfig(); err != nil {
			return fmt.Errorf("TLS configuration invalid: %v", err)
		}
	}

	g.recordHealthCheckMetricsInternal(true)

	return nil
}

// checkPortAvailability checks if the configured port is reachable.
// Successful and failed checks are cached briefly to avoid hammering health-check ports.
func (g *Service) checkPortAvailability() error {
	if g.conf == nil || g.conf.Addr == "" {
		return fmt.Errorf("server address not configured")
	}

	// Normalize a bind address (e.g. ":9090") to a dialable one (127.0.0.1:9090).
	addr := g.conf.Addr
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %v", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	norm := net.JoinHostPort(host, port)

	// A TCP dial probe only makes sense for TCP; skip for unix sockets etc.
	if g.conf.Network != "" && g.conf.Network != "tcp" {
		return nil
	}

	g.portCheckCache.mu.RLock()
	lastFailure := g.portCheckCache.lastFailure
	lastSuccess := g.portCheckCache.lastSuccess
	lastError := g.portCheckCache.lastError
	retryWindow := g.portCheckCache.retryWindow
	successWindow := g.portCheckCache.successWindow
	g.portCheckCache.mu.RUnlock()

	now := time.Now()
	if !lastSuccess.IsZero() && successWindow > 0 && now.Sub(lastSuccess) < successWindow {
		return nil
	}
	// If we have a recent failure within retry window, return cached error
	// This avoids hammering unreachable ports while still checking frequently
	if lastError != nil && !lastFailure.IsZero() && now.Sub(lastFailure) < retryWindow {
		return fmt.Errorf("port %s is not reachable (cached): %v", norm, lastError)
	}

	// Perform actual network check after the short success/failure cache expires.
	conn, err := net.DialTimeout("tcp", norm, 2*time.Second)
	if err != nil {
		// Cache failure to avoid frequent retries
		g.portCheckCache.mu.Lock()
		g.portCheckCache.lastFailure = now
		g.portCheckCache.lastSuccess = time.Time{}
		g.portCheckCache.lastError = err
		g.portCheckCache.mu.Unlock()
		return fmt.Errorf("port %s is not reachable: %v", norm, err)
	}
	_ = conn.Close()

	// Clear failure cache on success and remember success briefly.
	g.portCheckCache.mu.Lock()
	g.portCheckCache.lastFailure = time.Time{} // Clear failure cache
	g.portCheckCache.lastSuccess = now
	g.portCheckCache.lastError = nil
	g.portCheckCache.mu.Unlock()

	return nil
}

// updateHealthServingStatus sets the overall health serving status based on
// the shared readiness resource published by the gRPC client plugin.
// If the readiness resource is not found, it defaults to SERVING to remain
// backward compatible. If the resource exists and is false, it reports NOT_SERVING.
func (g *Service) updateHealthServingStatus() {
	if g.healthServer == nil {
		log.Warnf("Health server is nil, cannot update serving status")
		return
	}

	status := grpc_health_v1.HealthCheckResponse_SERVING
	if g.rt != nil {
		val, err := g.getRequiredReadinessState()
		if err != nil {
			// Resource not found or error accessing it - log only once to avoid spam
			// This is normal when gRPC client plugin is not used or no required upstreams are configured
			g.readinessResourceLogged.mu.RLock()
			alreadyLogged := g.readinessResourceLogged.value
			g.readinessResourceLogged.mu.RUnlock()

			if !alreadyLogged {
				// Check if error is "resource not found" (normal case) vs other errors
				errStr := err.Error()
				if strings.Contains(errStr, "resource not found") {
					// This is expected when gRPC client plugin is not used - only log once at DEBUG level
					log.Debugf("Shared readiness resource not found (this is normal if gRPC client plugin is not used), defaulting to SERVING")
				} else {
					// Other errors should be logged as they might indicate a real issue
					log.Debugf("Could not get shared readiness resource: %v, defaulting to SERVING", err)
				}

				g.readinessResourceLogged.mu.Lock()
				g.readinessResourceLogged.value = true
				g.readinessResourceLogged.mu.Unlock()
			}
		} else {
			// Resource exists - reset logged flag in case it was previously not found
			g.readinessResourceLogged.mu.Lock()
			g.readinessResourceLogged.value = false
			g.readinessResourceLogged.mu.Unlock()

			if ready, ok := val.(bool); ok {
				if !ready {
					status = grpc_health_v1.HealthCheckResponse_NOT_SERVING
					log.Debugf("Upstream readiness is false, setting status to NOT_SERVING")
				}
			} else {
				// Type assertion failed - log warning but use default SERVING status
				log.Warnf("Shared readiness resource has unexpected type: %T, expected bool, defaulting to SERVING", val)
			}
		}
	} else {
		log.Debugf("Runtime is nil, defaulting to SERVING status")
	}

	g.healthServer.SetServingStatus("", status)
}

func (g *Service) getRequiredReadinessState() (any, error) {
	if g == nil || g.rt == nil {
		return nil, fmt.Errorf("runtime is nil")
	}
	var lastErr error
	for _, resourceName := range []string{requiredReadinessStableResourceName, requiredReadinessResourceName} {
		val, err := g.rt.GetSharedResource(resourceName)
		if err == nil {
			return val, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// validateTLSConfig validates TLS configuration
func (g *Service) validateTLSConfig() error {
	if !g.conf.GetTlsEnable() {
		return nil
	}

	// Check TLS auth type is valid
	authType := g.conf.GetTlsAuthType()
	if authType < 0 || authType > 4 {
		return fmt.Errorf("invalid TLS auth type: %d", authType)
	}

	// Check if certificate provider is available
	var cp lynx.CertificateProvider
	if prov := g.getCertProvider(); prov != nil {
		if typed, ok := prov.(lynx.CertificateProvider); ok {
			cp = typed
		}
	}
	if cp == nil && lynx.Lynx() != nil {
		cp = lynx.Lynx().Certificate()
	}
	if cp == nil {
		return fmt.Errorf("certificate provider not configured")
	}

	// Validate certificate data presence
	if len(cp.GetCertificate()) == 0 {
		return fmt.Errorf("server certificate not provided")
	}
	if len(cp.GetPrivateKey()) == 0 {
		return fmt.Errorf("server private key not provided")
	}

	return nil
}

// validateConfig validates the gRPC server configuration
func (g *Service) validateConfig() error {
	if g.conf == nil {
		return fmt.Errorf("configuration is nil")
	}

	if g.conf.Network != "" && g.conf.Network != "tcp" && g.conf.Network != "unix" {
		return fmt.Errorf("unsupported network type: %s, supported types are 'tcp' and 'unix'", g.conf.Network)
	}

	if err := g.validateAddress(g.conf.Addr); err != nil {
		return fmt.Errorf("invalid address format: %v", err)
	}

	if g.conf.GetTlsEnable() {
		if err := g.validateTLSConfig(); err != nil {
			return fmt.Errorf("invalid TLS configuration: %v", err)
		}
	}

	if g.conf.Timeout != nil && g.conf.Timeout.AsDuration() <= 0 {
		return fmt.Errorf("timeout must be positive, got: %v", g.conf.Timeout.AsDuration())
	}

	// 0 means unlimited (valid); only warn on bounds that likely signal misconfiguration.
	if maxStreams := g.conf.GetMaxConcurrentStreams(); maxStreams > 0 {
		if maxStreams > 100000 {
			log.Warnf("MaxConcurrentStreams is very large (%d), this may consume excessive server resources", maxStreams)
		}
		if maxStreams < 10 {
			log.Warnf("MaxConcurrentStreams is very small (%d), this may unnecessarily limit concurrent requests", maxStreams)
		}
	}

	const maxAllowedMsgSize = 200 * 1024 * 1024 // 200MB; above this, warn about memory use.
	if recv := g.conf.GetMaxRecvMsgSize(); recv > 0 {
		if recv > maxAllowedMsgSize {
			log.Warnf("MaxRecvMsgSize is very large (%d bytes), this may consume excessive memory", recv)
		}
	}
	if send := g.conf.GetMaxSendMsgSize(); send > 0 {
		if send > maxAllowedMsgSize {
			log.Warnf("MaxSendMsgSize is very large (%d bytes), this may consume excessive memory", send)
		}
	}

	return nil
}

// validateAddress validates the server address format
func (g *Service) validateAddress(addr string) error {
	if addr == "" {
		return fmt.Errorf("address cannot be empty")
	}

	// TCP addresses must carry a port; an empty host means "all interfaces".
	if g.conf.Network == "tcp" || g.conf.Network == "" {
		if !strings.Contains(addr, ":") {
			return fmt.Errorf("TCP address must include port (e.g., ':9090' or 'localhost:9090')")
		}

		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("invalid address format: %v", err)
		}

		if port == "" {
			return fmt.Errorf("port cannot be empty")
		}

		// A non-resolving host is only a warning, not a hard failure (DNS may not be ready yet).
		if host != "" {
			if _, err := net.LookupHost(host); err != nil {
				log.Warn("Warning: could not resolve hostname", "host", host, "error", err)
			}
		}
	}

	return nil
}

// getAppName returns the application name using dependency injection
func (g *Service) getAppName() string {
	if g.appNameProvider != nil {
		return g.appNameProvider()
	}
	return "lynx" // fallback default
}

// getCertProvider returns the certificate provider using dependency injection.
func (g *Service) getCertProvider() any {
	if g.certProvider != nil {
		return g.certProvider()
	}
	return nil // fallback
}

// getControlPlane returns the control plane using dependency injection.
func (g *Service) getControlPlane() any {
	if g.controlPlaneProvider != nil {
		if cp := g.controlPlaneProvider(); cp != nil {
			return cp
		}
	}
	if app := lynx.Lynx(); app != nil {
		return app.GetControlPlane()
	}
	return nil
}

// getGRPCRateLimit returns the gRPC rate limit middleware using dependency injection
func (g *Service) getGRPCRateLimit() middleware.Middleware {
	controlPlane := g.getControlPlane()
	if controlPlane == nil {
		return nil
	}
	if rl, ok := controlPlane.(lynx.RateLimiter); ok {
		return rl.GRPCRateLimit()
	}
	return nil
}
