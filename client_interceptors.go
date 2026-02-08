// Package grpc provides gRPC client interceptors for tracing, metrics, circuit breaker, and logging.
// These run at the gRPC call level so that trace context is injected into metadata and metrics use the real method name.
package grpc

import (
	"context"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// metadataCarrier adapts gRPC metadata.MD to propagation.TextMapCarrier for trace context injection.
type metadataCarrier struct {
	md metadata.MD
}

func (m metadataCarrier) Get(key string) string {
	v := m.md.Get(key)
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func (m metadataCarrier) Set(key, value string) {
	m.md.Set(key, value)
}

func (m metadataCarrier) Keys() []string {
	out := make([]string, 0, len(m.md))
	for k := range m.md {
		out = append(out, k)
	}
	return out
}

// tracingUnaryClientInterceptor injects the current trace context into gRPC outgoing metadata
// so that the server can continue the same trace. Uses the global TextMapPropagator (e.g. set by lynx-tracer).
func tracingUnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		prop := otel.GetTextMapPropagator()
		if prop == nil {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			md = metadata.MD{}
		}
		// Copy so we don't modify the original
		mdCopy := md.Copy()
		prop.Inject(ctx, metadataCarrier{md: mdCopy})
		outCtx := metadata.NewOutgoingContext(ctx, mdCopy)
		return invoker(outCtx, method, req, reply, cc, opts...)
	}
}

// metricsUnaryClientInterceptor records per-method request count and duration.
func (c *ClientPlugin) metricsUnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		duration := time.Since(start)
		status := "success"
		if err != nil {
			status = "error"
		}
		serviceName := "unknown"
		if cc.Target() != "" {
			serviceName = cc.Target()
		}
		if c.metrics != nil {
			c.metrics.RecordRequest(serviceName, method, status, duration)
		}
		return err
	}
}

// circuitBreakerUnaryClientInterceptor runs the RPC inside circuitBreaker.Execute so that
// failures are counted and the circuit opens when the threshold is reached.
func circuitBreakerUnaryClientInterceptor(cb *CircuitBreaker) grpc.UnaryClientInterceptor {
	if cb == nil {
		return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
	}
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		var rpcErr error
		err := cb.Execute(ctx, func(ctx context.Context) error {
			rpcErr = invoker(ctx, method, req, reply, cc, opts...)
			return rpcErr
		})
		if err != nil && rpcErr != nil {
			return rpcErr
		}
		return err
	}
}

// loggingUnaryClientInterceptor logs each RPC with method, duration, and trace id when available.
func (c *ClientPlugin) loggingUnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		duration := time.Since(start)
		traceID := ""
		if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
			traceID = span.SpanContext().TraceID().String()
		}
		if err != nil {
			if traceID != "" {
				log.Warnf("[gRPC Client] method=%s target=%s duration=%v trace_id=%s error=%v", method, cc.Target(), duration, traceID, err)
			} else {
				log.Warnf("[gRPC Client] method=%s target=%s duration=%v error=%v", method, cc.Target(), duration, err)
			}
		} else {
			if traceID != "" {
				log.Debugf("[gRPC Client] method=%s target=%s duration=%v trace_id=%s", method, cc.Target(), duration, traceID)
			} else {
				log.Debugf("[gRPC Client] method=%s target=%s duration=%v", method, cc.Target(), duration)
			}
		}
		return err
	}
}

// retryUnaryClientInterceptor retries failed unary RPCs when the error is retryable (Unavailable, DeadlineExceeded, etc.).
func (c *ClientPlugin) retryUnaryClientInterceptor(config ClientConfig) grpc.UnaryClientInterceptor {
	maxRetries := int(config.MaxRetries)
	if maxRetries <= 0 && c.conf != nil && c.conf.MaxRetries > 0 {
		maxRetries = int(c.conf.MaxRetries)
	}
	if maxRetries <= 0 {
		maxRetries = 3
	}
	baseDelay := config.RetryBackoff
	if baseDelay <= 0 && c.conf != nil && c.conf.RetryBackoff != nil {
		baseDelay = c.conf.RetryBackoff.AsDuration()
	}
	if baseDelay <= 0 {
		baseDelay = time.Second
	}
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		var lastErr error
		for attempt := 0; attempt <= maxRetries; attempt++ {
			lastErr = invoker(ctx, method, req, reply, cc, opts...)
			if lastErr == nil {
				return nil
			}
			if !c.isRetryableError(lastErr) {
				return lastErr
			}
			if attempt == maxRetries {
				return lastErr
			}
			delay := c.calculateRetryDelay(attempt, baseDelay, 5*time.Second)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		return lastErr
	}
}

// buildClientInterceptorChain returns the list of UnaryClientInterceptors to apply for the given config.
// Order: tracing, retry (if maxRetries > 0), metrics, circuit breaker (if enabled), logging.
func (c *ClientPlugin) buildClientInterceptorChain(config ClientConfig) []grpc.UnaryClientInterceptor {
	var chain []grpc.UnaryClientInterceptor

	if c.conf != nil && c.conf.GetTracingEnabled() {
		chain = append(chain, tracingUnaryClientInterceptor())
	}

	maxRetries := int(config.MaxRetries)
	if maxRetries <= 0 && c.conf != nil && c.conf.MaxRetries > 0 {
		maxRetries = int(c.conf.MaxRetries)
	}
	if maxRetries > 0 {
		chain = append(chain, c.retryUnaryClientInterceptor(config))
	}

	chain = append(chain, c.metricsUnaryClientInterceptor())

	if config.CircuitBreaker {
		cbConfig := &CircuitBreakerConfig{
			Enabled:               config.CircuitBreaker,
			FailureThreshold:      config.CircuitThreshold,
			RecoveryTimeout:       30 * time.Second,
			SuccessThreshold:      3,
			Timeout:               config.Timeout,
			MaxConcurrentRequests: 10,
		}
		cb := c.circuitBreakers.GetCircuitBreaker(config.ServiceName, cbConfig)
		chain = append(chain, circuitBreakerUnaryClientInterceptor(cb))
	}

	chain = append(chain, c.loggingUnaryClientInterceptor())

	return chain
}

// tracingStreamClientInterceptor injects trace context into stream outgoing metadata.
func tracingStreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		prop := otel.GetTextMapPropagator()
		if prop == nil {
			return streamer(ctx, desc, cc, method, opts...)
		}
		md, _ := metadata.FromOutgoingContext(ctx)
		if md == nil {
			md = metadata.MD{}
		}
		mdCopy := md.Copy()
		prop.Inject(ctx, metadataCarrier{md: mdCopy})
		outCtx := metadata.NewOutgoingContext(ctx, mdCopy)
		return streamer(outCtx, desc, cc, method, opts...)
	}
}

// metricsStreamClientInterceptor records stream RPC metrics.
func (c *ClientPlugin) metricsStreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		start := time.Now()
		cs, err := streamer(ctx, desc, cc, method, opts...)
		duration := time.Since(start)
		status := "success"
		if err != nil {
			status = "error"
		}
		target := cc.Target()
		if target == "" {
			target = "unknown"
		}
		if c.metrics != nil {
			c.metrics.RecordRequest(target, method, status, duration)
		}
		return cs, err
	}
}

// loggingStreamClientInterceptor logs stream RPC start/finish.
func (c *ClientPlugin) loggingStreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		start := time.Now()
		cs, err := streamer(ctx, desc, cc, method, opts...)
		duration := time.Since(start)
		traceID := "none"
		if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
			traceID = span.SpanContext().TraceID().String()
		}
		if err != nil {
			log.Warnf("[gRPC Client Stream] method=%s target=%s duration=%v trace_id=%s error=%v", method, cc.Target(), duration, traceID, err)
		} else {
			log.Debugf("[gRPC Client Stream] method=%s target=%s duration=%v trace_id=%s", method, cc.Target(), duration, traceID)
		}
		return cs, err
	}
}

// buildClientStreamInterceptorChain returns StreamClientInterceptors: tracing (if enabled), metrics, logging.
func (c *ClientPlugin) buildClientStreamInterceptorChain(config ClientConfig) []grpc.StreamClientInterceptor {
	var chain []grpc.StreamClientInterceptor
	if c.conf != nil && c.conf.GetTracingEnabled() {
		chain = append(chain, tracingStreamClientInterceptor())
	}
	chain = append(chain, c.metricsStreamClientInterceptor())
	chain = append(chain, c.loggingStreamClientInterceptor())
	return chain
}
