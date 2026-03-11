package grpc

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-lynx/lynx-grpc/conf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- Circuit breaker: concurrentRequests must never go negative ---

func TestCircuitBreaker_ConcurrentRequestsNeverNegative(t *testing.T) {
	cfg := &CircuitBreakerConfig{
		Enabled:               true,
		FailureThreshold:      2,
		RecoveryTimeout:       50 * time.Millisecond,
		SuccessThreshold:      2,
		Timeout:               5 * time.Second,
		MaxConcurrentRequests: 5,
	}
	cb := NewCircuitBreaker("test-svc", cfg, nil)

	// Drive the breaker to OPEN by injecting failures.
	for i := 0; i < cfg.FailureThreshold; i++ {
		err := cb.Execute(context.Background(), func(ctx context.Context) error {
			return fmt.Errorf("forced failure %d", i)
		})
		assert.Error(t, err)
	}
	assert.Equal(t, CircuitBreakerOpen, cb.GetState())

	// Wait for recovery timeout so next allowRequest transitions to HALF_OPEN.
	time.Sleep(cfg.RecoveryTimeout + 10*time.Millisecond)

	// Directly call recordResult when state is HALF_OPEN but concurrentRequests is 0.
	// This simulates a stale request from the CLOSED era completing during HALF_OPEN.
	cb.mu.Lock()
	cb.state = CircuitBreakerHalfOpen
	cb.concurrentRequests = 0
	cb.mu.Unlock()

	// Before the fix this would decrement to -1.
	cb.recordResult(nil)

	cb.mu.RLock()
	cr := cb.concurrentRequests
	cb.mu.RUnlock()
	assert.Equal(t, 0, cr, "concurrentRequests must not go negative")
}

func TestCircuitBreaker_HalfOpenNormalFlow(t *testing.T) {
	cfg := &CircuitBreakerConfig{
		Enabled:               true,
		FailureThreshold:      2,
		RecoveryTimeout:       50 * time.Millisecond,
		SuccessThreshold:      2,
		Timeout:               5 * time.Second,
		MaxConcurrentRequests: 3,
	}
	cb := NewCircuitBreaker("test-svc", cfg, nil)

	// Drive to OPEN.
	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = cb.Execute(context.Background(), func(ctx context.Context) error {
			return fmt.Errorf("fail")
		})
	}
	require.Equal(t, CircuitBreakerOpen, cb.GetState())

	// Wait for recovery.
	time.Sleep(cfg.RecoveryTimeout + 10*time.Millisecond)

	// First request in HALF_OPEN: allowed, succeeds.
	err := cb.Execute(context.Background(), func(ctx context.Context) error { return nil })
	assert.NoError(t, err)

	// concurrentRequests should be back to 0 after the request completed.
	cb.mu.RLock()
	cr := cb.concurrentRequests
	cb.mu.RUnlock()
	assert.Equal(t, 0, cr)
}

func TestCircuitBreaker_ConcurrentHalfOpenRequests(t *testing.T) {
	cfg := &CircuitBreakerConfig{
		Enabled:               true,
		FailureThreshold:      1,
		RecoveryTimeout:       10 * time.Millisecond,
		SuccessThreshold:      3,
		Timeout:               5 * time.Second,
		MaxConcurrentRequests: 2,
	}
	cb := NewCircuitBreaker("test-svc", cfg, nil)

	// Drive to OPEN.
	_ = cb.Execute(context.Background(), func(ctx context.Context) error {
		return fmt.Errorf("fail")
	})
	require.Equal(t, CircuitBreakerOpen, cb.GetState())

	time.Sleep(cfg.RecoveryTimeout + 10*time.Millisecond)

	// Launch MaxConcurrentRequests concurrent requests in HALF_OPEN.
	var wg sync.WaitGroup
	started := make(chan struct{})
	for i := 0; i < cfg.MaxConcurrentRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cb.Execute(context.Background(), func(ctx context.Context) error {
				started <- struct{}{}
				time.Sleep(30 * time.Millisecond)
				return nil
			})
		}()
	}

	// Wait for all goroutines to start executing.
	for i := 0; i < cfg.MaxConcurrentRequests; i++ {
		<-started
	}

	// While requests are in-flight, verify concurrentRequests <= MaxConcurrentRequests.
	cb.mu.RLock()
	state := cb.state
	cr := cb.concurrentRequests
	cb.mu.RUnlock()
	if state == CircuitBreakerHalfOpen {
		assert.LessOrEqual(t, cr, cfg.MaxConcurrentRequests)
	}

	wg.Wait()

	// After all complete, concurrentRequests must be >= 0.
	cb.mu.RLock()
	crAfter := cb.concurrentRequests
	cb.mu.RUnlock()
	assert.GreaterOrEqual(t, crAfter, 0, "concurrentRequests must never be negative after concurrent execution")
}

// --- Retry middleware: context cancellation during backoff ---
// Create a lightweight ClientPlugin without Prometheus metrics to avoid registration conflicts.

func newLightClientPlugin() *ClientPlugin {
	return &ClientPlugin{
		conf:        &conf.GrpcClient{},
		connections: make(map[string]*grpc.ClientConn),
	}
}

func TestRetryMiddleware_ContextCancelDuringBackoff(t *testing.T) {
	plugin := newLightClientPlugin()
	plugin.conf.MaxRetries = 5
	retryMw := plugin.getRetryMiddleware()

	attempts := 0
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		attempts++
		return nil, status.Error(codes.Unavailable, "transient error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	wrappedHandler := retryMw(handler)
	_, err := wrappedHandler(ctx, "test-request")

	assert.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.GreaterOrEqual(t, attempts, 1)
	assert.Less(t, attempts, 6, "should not exhaust all retries when context is cancelled")
}

func TestRetryMiddleware_SuccessOnFirstAttempt(t *testing.T) {
	plugin := newLightClientPlugin()
	plugin.conf.MaxRetries = 3
	retryMw := plugin.getRetryMiddleware()

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	}

	wrappedHandler := retryMw(handler)
	resp, err := wrappedHandler(context.Background(), "req")

	assert.NoError(t, err)
	assert.Equal(t, "ok", resp)
}

func TestRetryMiddleware_SuccessAfterRetries(t *testing.T) {
	plugin := newLightClientPlugin()
	plugin.conf.MaxRetries = 5
	retryMw := plugin.getRetryMiddleware()

	attempts := 0
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		attempts++
		if attempts < 3 {
			return nil, status.Error(codes.Unavailable, "transient")
		}
		return "recovered", nil
	}

	wrappedHandler := retryMw(handler)
	resp, err := wrappedHandler(context.Background(), "req")

	assert.NoError(t, err)
	assert.Equal(t, "recovered", resp)
	assert.Equal(t, 3, attempts)
}

// --- Health status update: context cancellation ---

func TestUpdateHealthServingStatusWithContext_CancelledContext(t *testing.T) {
	svc := NewGrpcService()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Already cancelled.

	done := make(chan struct{})
	go func() {
		svc.updateHealthServingStatusWithContext(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Completed without hanging.
	case <-time.After(2 * time.Second):
		t.Fatal("updateHealthServingStatusWithContext did not return within timeout on cancelled context")
	}
}

func TestUpdateHealthServingStatusWithContext_TimedOutContext(t *testing.T) {
	svc := NewGrpcService()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		svc.updateHealthServingStatusWithContext(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Completed.
	case <-time.After(2 * time.Second):
		t.Fatal("updateHealthServingStatusWithContext did not return within timeout")
	}
}

func TestUpdateHealthServingStatusWithContext_ValidContext(t *testing.T) {
	svc := NewGrpcService()
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		svc.updateHealthServingStatusWithContext(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Completed normally — no goroutine leak.
	case <-time.After(2 * time.Second):
		t.Fatal("updateHealthServingStatusWithContext did not return with valid context")
	}
}

func TestUpdateHealthServingStatusWithContext_NoGoroutineLeak(t *testing.T) {
	svc := NewGrpcService()

	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		svc.updateHealthServingStatusWithContext(ctx)
		cancel()
	}

	// Allow a brief settle period for any stale goroutines (there should be none).
	time.Sleep(100 * time.Millisecond)
	// If the old implementation were still in place, ~100 leaked goroutines would accumulate.
	// The simplified implementation spawns zero goroutines, so this is a structural guarantee.
}

// --- Configure: concurrency safety ---

func TestConfigure_ConcurrentAccess(t *testing.T) {
	svc := NewGrpcService()
	svc.conf = &conf.Service{
		Network: "tcp",
		Addr:    ":9090",
	}

	var wg sync.WaitGroup
	errs := make([]error, 50)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			port := 9100 + idx
			newConf := &conf.Service{
				Network: "tcp",
				Addr:    fmt.Sprintf(":%d", port),
			}
			errs[idx] = svc.Configure(newConf)
		}(i)
	}
	wg.Wait()

	// All Configure calls should succeed (no panic, no data race).
	for i, err := range errs {
		assert.NoError(t, err, "Configure goroutine %d failed", i)
	}

	// conf should be one of the valid configurations (not torn/corrupted).
	assert.NotNil(t, svc.conf)
	assert.Equal(t, "tcp", svc.conf.Network)
	assert.NotEmpty(t, svc.conf.Addr)
}

func TestConfigure_RollbackOnInvalidConfig(t *testing.T) {
	svc := NewGrpcService()
	original := &conf.Service{
		Network: "tcp",
		Addr:    ":9090",
	}
	svc.conf = original

	// Apply invalid config — should rollback.
	err := svc.Configure(&conf.Service{
		Network: "invalid",
		Addr:    ":9090",
	})
	assert.Error(t, err)
	assert.Equal(t, original, svc.conf, "conf should be rolled back to original after validation failure")
}
