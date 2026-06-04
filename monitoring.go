// This file implements server-side Prometheus metrics, request logging, and interceptor chains
// for the gRPC service plugin (rate limit, in-flight, circuit breaker, metrics, logging).
package grpc

import (
	"context"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

var (
	grpcServerUp = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "lynx",
			Subsystem: "grpc",
			Name:      "server_up",
			Help:      "Whether the gRPC server is up",
		},
		[]string{"server_name", "address"},
	)

	grpcRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "lynx",
			Subsystem: "grpc",
			Name:      "requests_total",
			Help:      "Total number of gRPC requests",
		},
		[]string{"method", "status"},
	)

	grpcRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "lynx",
			Subsystem: "grpc",
			Name:      "request_duration_seconds",
			Help:      "Duration of gRPC requests",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"method"},
	)

	grpcActiveConnections = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "lynx",
			Subsystem: "grpc",
			Name:      "active_connections",
			Help:      "Number of active gRPC connections",
		},
		[]string{"server_name"},
	)

	grpcServerStartTime = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "lynx",
			Subsystem: "grpc",
			Name:      "server_start_time_seconds",
			Help:      "Unix timestamp of gRPC server start time",
		},
		[]string{"server_name"},
	)

	grpcServerErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "lynx",
			Subsystem: "grpc",
			Name:      "server_errors_total",
			Help:      "Total number of gRPC server errors",
		},
		[]string{"error_type"},
	)
)

func (g *Service) recordHealthCheckMetricsInternal(healthy bool) {
	if g.conf == nil {
		return
	}

	if healthy {
		grpcServerUp.WithLabelValues(g.getAppName(), g.conf.Addr).Set(1)
	} else {
		grpcServerUp.WithLabelValues(g.getAppName(), g.conf.Addr).Set(0)
	}
}

func (g *Service) recordRequestMetrics(method string, duration time.Duration, status string) {
	grpcRequestsTotal.WithLabelValues(method, status).Inc()
	grpcRequestDuration.WithLabelValues(method).Observe(duration.Seconds())
}

func (g *Service) updateConnectionMetrics(active int) {
	grpcActiveConnections.WithLabelValues(g.getAppName()).Set(float64(active))
}

func (g *Service) recordServerStartTime() {
	grpcServerStartTime.WithLabelValues(g.getAppName()).Set(float64(time.Now().Unix()))
}

func (g *Service) recordServerError(errorType string) {
	grpcServerErrors.WithLabelValues(errorType).Inc()
}

// getMetricsHandler returns a gRPC UnaryServerInterceptor that records per-method metrics (FullMethod).
func (g *Service) getMetricsHandler() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		duration := time.Since(start)
		status := "success"
		if err != nil {
			status = "error"
			g.recordServerError("request_error")
		}
		g.recordRequestMetrics(info.FullMethod, duration, status)
		return resp, err
	}
}

// traceIDForLog returns the trace id from context, or "none" when invalid/empty (avoids misleading all-zero ids).
func traceIDForLog(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if !span.SpanContext().IsValid() {
		return "none"
	}
	return span.SpanContext().TraceID().String()
}

// peerAddrFromContext returns the peer address from gRPC context, or "unknown".
func peerAddrFromContext(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil || p.Addr == nil {
		return "unknown"
	}
	return p.Addr.String()
}

// getServerLoggingInterceptor returns a gRPC UnaryServerInterceptor that logs each request and sets Trace-Id/Span-Id in trailing metadata.
func (g *Service) getServerLoggingInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		duration := time.Since(start)
		span := trace.SpanFromContext(ctx)
		traceID := "none"
		spanID := "none"
		if span.SpanContext().IsValid() {
			traceID = span.SpanContext().TraceID().String()
			spanID = span.SpanContext().SpanID().String()
		}
		// Set Trace-Id and Span-Id in trailing metadata so clients can correlate.
		_ = grpc.SetTrailer(ctx, metadata.Pairs("trace-id", traceID, "span-id", spanID))
		peerAddr := peerAddrFromContext(ctx)
		if err != nil {
			log.Context(ctx).Errorf("[gRPC Server] method=%s peer=%s duration=%v trace_id=%s error=%v", info.FullMethod, peerAddr, duration, traceID, err)
		} else {
			log.Context(ctx).Infof("[gRPC Server] method=%s peer=%s duration=%v trace_id=%s", info.FullMethod, peerAddr, duration, traceID)
		}
		return resp, err
	}
}

// getRateLimitUnaryInterceptor returns a UnaryServerInterceptor that enforces rate limit when serverOpts has a limiter.
func (g *Service) getRateLimitUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if g.serverOpts == nil || g.serverOpts.limiter == nil {
			return handler(ctx, req)
		}
		if err := g.serverOpts.limiter.Wait(ctx); err != nil {
			g.recordServerError("rate_limit")
			return nil, err
		}
		return handler(ctx, req)
	}
}

// getInflightUnaryInterceptor returns a UnaryServerInterceptor that limits concurrent unary RPCs when serverOpts has a semaphore.
func (g *Service) getInflightUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if g.serverOpts == nil || g.serverOpts.inflightSem == nil {
			return handler(ctx, req)
		}
		select {
		case g.serverOpts.inflightSem <- struct{}{}:
			defer func() { <-g.serverOpts.inflightSem }()
			return handler(ctx, req)
		case <-ctx.Done():
			g.recordServerError("inflight_limit")
			return nil, ctx.Err()
		}
	}
}

// getServerCircuitBreakerUnaryInterceptor returns a UnaryServerInterceptor that opens the circuit after failure threshold and returns Unavailable when open.
func (g *Service) getServerCircuitBreakerUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if g.serverOpts == nil || g.serverOpts.serverCircuitBreaker == nil {
			return handler(ctx, req)
		}
		cb := g.serverOpts.serverCircuitBreaker
		var resp any
		var rpcErr error
		err := cb.Execute(ctx, func(ctx context.Context) error {
			resp, rpcErr = handler(ctx, req)
			return rpcErr
		})
		if err != nil && resp == nil && rpcErr == nil {
			g.recordServerError("circuit_breaker_open")
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return resp, rpcErr
	}
}

// getServerInterceptorChain returns the native gRPC UnaryServerInterceptors in order: rate limit, inflight, circuit breaker (if enabled), metrics (if enabled), logging (if enabled).
func (g *Service) getServerInterceptorChain() []grpc.UnaryServerInterceptor {
	var chain []grpc.UnaryServerInterceptor
	if g.serverOpts != nil {
		if g.serverOpts.limiter != nil {
			chain = append(chain, g.getRateLimitUnaryInterceptor())
		}
		if g.serverOpts.inflightSem != nil {
			chain = append(chain, g.getInflightUnaryInterceptor())
		}
		if g.serverOpts.serverCircuitBreaker != nil {
			chain = append(chain, g.getServerCircuitBreakerUnaryInterceptor())
		}
		if g.serverOpts.opts.EnableMetrics {
			chain = append(chain, g.getMetricsHandler())
		}
		if g.serverOpts.opts.EnableRequestLogging {
			chain = append(chain, g.getServerLoggingInterceptor())
		}
	} else {
		chain = []grpc.UnaryServerInterceptor{g.getMetricsHandler(), g.getServerLoggingInterceptor()}
	}
	return chain
}

// getServerCircuitBreakerStreamInterceptor returns a StreamServerInterceptor that applies the same circuit breaker to stream RPCs.
func (g *Service) getServerCircuitBreakerStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if g.serverOpts == nil || g.serverOpts.serverCircuitBreaker == nil {
			return handler(srv, ss)
		}
		cb := g.serverOpts.serverCircuitBreaker
		var streamErr error
		err := cb.Execute(ss.Context(), func(ctx context.Context) error {
			streamErr = handler(srv, ss)
			return streamErr
		})
		if err != nil && streamErr == nil {
			g.recordServerError("circuit_breaker_open")
			return status.Error(codes.Unavailable, err.Error())
		}
		return streamErr
	}
}

// getStreamMetricsInterceptor returns a StreamServerInterceptor that records stream RPC metrics.
func (g *Service) getStreamMetricsInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		duration := time.Since(start)
		status := "success"
		if err != nil {
			status = "error"
			g.recordServerError("stream_error")
		}
		g.recordRequestMetrics(info.FullMethod, duration, status)
		return err
	}
}

// getStreamLoggingInterceptor returns a StreamServerInterceptor that logs stream start/finish and sets trace id in trailing metadata.
func (g *Service) getStreamLoggingInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		start := time.Now()
		err := handler(srv, ss)
		duration := time.Since(start)
		traceID := traceIDForLog(ctx)
		span := trace.SpanFromContext(ctx)
		spanID := "none"
		if span.SpanContext().IsValid() {
			spanID = span.SpanContext().SpanID().String()
		}
		_ = grpc.SetTrailer(ctx, metadata.Pairs("trace-id", traceID, "span-id", spanID))
		peerAddr := peerAddrFromContext(ctx)
		if err != nil {
			log.Context(ctx).Errorf("[gRPC Server Stream] method=%s peer=%s duration=%v trace_id=%s error=%v", info.FullMethod, peerAddr, duration, traceID, err)
		} else {
			log.Context(ctx).Infof("[gRPC Server Stream] method=%s peer=%s duration=%v trace_id=%s", info.FullMethod, peerAddr, duration, traceID)
		}
		return err
	}
}

// getServerStreamInterceptorChain returns StreamServerInterceptors: circuit breaker (if enabled), metrics (if enabled), logging (if enabled).
func (g *Service) getServerStreamInterceptorChain() []grpc.StreamServerInterceptor {
	var chain []grpc.StreamServerInterceptor
	if g.serverOpts != nil {
		if g.serverOpts.serverCircuitBreaker != nil {
			chain = append(chain, g.getServerCircuitBreakerStreamInterceptor())
		}
		if g.serverOpts.opts.EnableMetrics {
			chain = append(chain, g.getStreamMetricsInterceptor())
		}
		if g.serverOpts.opts.EnableRequestLogging {
			chain = append(chain, g.getStreamLoggingInterceptor())
		}
	} else {
		chain = []grpc.StreamServerInterceptor{g.getStreamMetricsInterceptor(), g.getStreamLoggingInterceptor()}
	}
	return chain
}
