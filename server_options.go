// Package grpc defines server-side options that extend the proto-based conf.Service.
// These are loaded from the same config key (lynx.grpc.service) so YAML can set them without proto changes.
package grpc

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ServerOptions holds optional server settings (graceful shutdown, middleware toggles, rate limit, in-flight limit, circuit breaker).
// Loaded from config under lynx.grpc.service with json/yaml tags.
type ServerOptions struct {
	// GracefulShutdownTimeout is the max time to wait for the server to drain. Default 30s.
	GracefulShutdownTimeout time.Duration `json:"graceful_shutdown_timeout" yaml:"graceful_shutdown_timeout"`
	// EnableTracing turns on Kratos tracing middleware. Default true.
	EnableTracing bool `json:"enable_tracing" yaml:"enable_tracing"`
	// EnableRequestLogging turns on the request logging interceptor. Default true.
	EnableRequestLogging bool `json:"enable_request_logging" yaml:"enable_request_logging"`
	// EnableMetrics turns on the metrics interceptor. Default true.
	EnableMetrics bool `json:"enable_metrics" yaml:"enable_metrics"`
	// RateLimit configures in-process rate limiting. When enabled, limits requests per second.
	RateLimit RateLimitConfig `json:"rate_limit" yaml:"rate_limit"`
	// MaxInflightUnary caps the number of concurrent unary RPCs. 0 means unlimited. Default 0.
	MaxInflightUnary int32 `json:"max_inflight_unary" yaml:"max_inflight_unary"`
	// CircuitBreaker configures server-side circuit breaker. When open, new requests get Unavailable.
	CircuitBreaker ServerCircuitBreakerConfig `json:"circuit_breaker" yaml:"circuit_breaker"`
}

// ServerCircuitBreakerConfig is the server circuit breaker configuration.
type ServerCircuitBreakerConfig struct {
	Enabled               bool          `json:"enabled" yaml:"enabled"`
	FailureThreshold      int           `json:"failure_threshold" yaml:"failure_threshold"`
	RecoveryTimeout       time.Duration `json:"recovery_timeout" yaml:"recovery_timeout"`
	SuccessThreshold      int           `json:"success_threshold" yaml:"success_threshold"`
	Timeout               time.Duration `json:"timeout" yaml:"timeout"`
	MaxConcurrentRequests int           `json:"max_concurrent_requests" yaml:"max_concurrent_requests"`
}

// RateLimitConfig is the rate limiter configuration.
type RateLimitConfig struct {
	Enabled       bool    `json:"enabled" yaml:"enabled"`
	RatePerSecond float64 `json:"rate_per_second" yaml:"rate_per_second"`
	Burst         int     `json:"burst" yaml:"burst"`
}

// serverOptsWithLimiter holds the parsed options and the constructed rate.Limiter / circuit breaker.
type serverOptsWithLimiter struct {
	opts    ServerOptions
	limiter *rate.Limiter
	// inflightSem is a semaphore for max concurrent unary requests; nil when MaxInflightUnary <= 0.
	inflightSem chan struct{}
	// serverCircuitBreaker is the server-side circuit breaker; nil when not enabled.
	serverCircuitBreaker *CircuitBreaker
}

// defaults for ServerOptions.
func defaultServerOptions() ServerOptions {
	return ServerOptions{
		GracefulShutdownTimeout: 30 * time.Second,
		EnableTracing:           true,
		EnableRequestLogging:    true,
		EnableMetrics:           true,
		MaxInflightUnary:        0,
		CircuitBreaker: ServerCircuitBreakerConfig{
			Enabled:               false,
			FailureThreshold:      5,
			RecoveryTimeout:       30 * time.Second,
			SuccessThreshold:      3,
			Timeout:               10 * time.Second,
			MaxConcurrentRequests: 10,
		},
	}
}

// serverOptionsCache is the loaded and parsed server options (set in InitializeResources).
type serverOptionsCache struct {
	mu    sync.RWMutex
	value *serverOptsWithLimiter
}

func (c *serverOptionsCache) get() *serverOptsWithLimiter {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.value
}

func (c *serverOptionsCache) set(v *serverOptsWithLimiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = v
}
