package grpc

import (
	"time"

	"github.com/go-kratos/kratos/v2/registry"
	"github.com/go-lynx/lynx-grpc/conf"
	"golang.org/x/time/rate"
)

func buildServerOptionsFromConfig(cfg *conf.Service, opts ServerOptions) *serverOptsWithLimiter {
	sol := &serverOptsWithLimiter{opts: opts}
	if opts.RateLimit.Enabled && opts.RateLimit.RatePerSecond > 0 {
		rps := rate.Limit(opts.RateLimit.RatePerSecond)
		burst := opts.RateLimit.Burst
		if burst <= 0 {
			burst = int(opts.RateLimit.RatePerSecond) + 1
		}
		sol.limiter = rate.NewLimiter(rps, burst)
	}
	if opts.MaxInflightUnary > 0 {
		sol.inflightSem = make(chan struct{}, opts.MaxInflightUnary)
	}
	if opts.CircuitBreaker.Enabled {
		cbCfg := &CircuitBreakerConfig{
			Enabled:               true,
			FailureThreshold:      opts.CircuitBreaker.FailureThreshold,
			RecoveryTimeout:       opts.CircuitBreaker.RecoveryTimeout,
			SuccessThreshold:      opts.CircuitBreaker.SuccessThreshold,
			Timeout:               opts.CircuitBreaker.Timeout,
			MaxConcurrentRequests: opts.CircuitBreaker.MaxConcurrentRequests,
		}
		if cbCfg.FailureThreshold <= 0 {
			cbCfg.FailureThreshold = 5
		}
		if cbCfg.RecoveryTimeout <= 0 {
			cbCfg.RecoveryTimeout = 30 * time.Second
		}
		if cbCfg.SuccessThreshold <= 0 {
			cbCfg.SuccessThreshold = 3
		}
		if cbCfg.Timeout <= 0 {
			cbCfg.Timeout = 10 * time.Second
		}
		if cbCfg.MaxConcurrentRequests <= 0 {
			cbCfg.MaxConcurrentRequests = 10
		}
		sol.serverCircuitBreaker = NewCircuitBreaker("grpc.server", cbCfg, nil)
	}
	_ = cfg
	return sol
}

func (g *Service) rebuildServerOptions() {
	if g == nil {
		return
	}
	opts := defaultServerOptions()
	g.serverOpts = buildServerOptionsFromConfig(g.conf, opts)
}

func (c *ClientPlugin) rebuildConnectionPoolLocked() {
	if c == nil || c.conf == nil {
		return
	}

	if c.connectionPool != nil {
		_ = c.connectionPool.CloseAll()
	}

	poolEnabled := c.conf.GetConnectionPooling()
	maxServices := int(c.conf.GetPoolSize())
	maxConnsPerService := int(c.conf.MaxConnections)
	idleTimeout := time.Duration(0)
	if c.conf.GetIdleTimeout() != nil {
		idleTimeout = c.conf.GetIdleTimeout().AsDuration()
	}
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

func (c *ClientPlugin) resolveServiceDiscoveryLocked() registry.Discovery {
	if c == nil {
		return nil
	}
	return c.resolveServiceDiscovery()
}
