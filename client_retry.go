// RetryHandler provides optional programmatic retry (e.g. for tests or custom flows).
// The default gRPC client retry is implemented in retryUnaryClientInterceptor (client_interceptors.go).
package grpc

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"

	"github.com/go-lynx/lynx/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RetryHandler retries an operation with exponential backoff. It is the optional
// programmatic path; production RPC retries go through retryUnaryClientInterceptor.
// A handler holds no per-call state, so a single instance is safe for concurrent use.
type RetryHandler struct {
	maxRetries        int
	retryBackoff      time.Duration
	maxBackoff        time.Duration
	backoffMultiplier float64
}

// NewRetryHandler creates a new retry handler
func NewRetryHandler() *RetryHandler {
	return &RetryHandler{
		maxRetries:        3,
		retryBackoff:      time.Second,
		maxBackoff:        30 * time.Second,
		backoffMultiplier: 2.0,
	}
}

// Initialize overrides max retry count and base backoff; maxBackoff and
// multiplier keep their constructor defaults.
func (r *RetryHandler) Initialize(maxRetries int, retryBackoff time.Duration) {
	r.maxRetries = maxRetries
	r.retryBackoff = retryBackoff
}

// ExecuteWithRetry executes a request with retry logic.
func (r *RetryHandler) ExecuteWithRetry(ctx context.Context, handler func(context.Context, any) (any, error), req any) (any, error) {
	var lastErr error
	backoff := r.retryBackoff

	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		resp, err := handler(ctx, req)
		if err == nil {
			if attempt > 0 {
				log.Infof("Request succeeded after %d retries", attempt)
			}
			return resp, nil
		}

		lastErr = err

		if !r.isRetryableError(err) {
			log.Debugf("Error is not retryable: %v", err)
			return nil, err
		}

		if attempt == r.maxRetries {
			log.Errorf("Request failed after %d retries: %v", r.maxRetries, err)
			return nil, err
		}

		log.Warnf("Request failed (attempt %d/%d), retrying in %v: %v",
			attempt+1, r.maxRetries+1, backoff, err)

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}

		// Grow backoff geometrically, capped at maxBackoff.
		backoff = time.Duration(float64(backoff) * r.backoffMultiplier)
		if backoff > r.maxBackoff {
			backoff = r.maxBackoff
		}
	}

	return nil, lastErr
}

// isRetryableError reports whether err is worth retrying. Transient/server-side
// codes (Unavailable, DeadlineExceeded, ResourceExhausted, Aborted, OutOfRange,
// Internal, DataLoss) are retryable; client-fault codes (Unauthenticated,
// PermissionDenied, InvalidArgument, NotFound, AlreadyExists, FailedPrecondition)
// are not, since retrying them cannot succeed. Local context cancellation/deadline
// is never retried. Unlike ClientPlugin.isRetryableError, this defaults non-gRPC
// errors to retryable.
func (r *RetryHandler) isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
			return true
		case codes.Unauthenticated, codes.PermissionDenied, codes.InvalidArgument:
			return false
		case codes.NotFound, codes.AlreadyExists, codes.FailedPrecondition:
			return false
		case codes.Aborted, codes.OutOfRange:
			return true
		case codes.Internal, codes.DataLoss:
			return true
		default:
			return false
		}
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}

	return true
}

// GetRetryConfig returns the current retry configuration
func (r *RetryHandler) GetRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:        r.maxRetries,
		RetryBackoff:      r.retryBackoff,
		MaxBackoff:        r.maxBackoff,
		BackoffMultiplier: r.backoffMultiplier,
	}
}

// SetRetryConfig updates the retry configuration
func (r *RetryHandler) SetRetryConfig(config RetryConfig) {
	r.maxRetries = config.MaxRetries
	r.retryBackoff = config.RetryBackoff
	r.maxBackoff = config.MaxBackoff
	r.backoffMultiplier = config.BackoffMultiplier
}

// RetryConfig represents retry configuration
type RetryConfig struct {
	MaxRetries        int
	RetryBackoff      time.Duration
	MaxBackoff        time.Duration
	BackoffMultiplier float64
}

// DefaultRetryConfig returns the defaults: 3 retries, 1s base backoff doubling up to 30s.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:        3,
		RetryBackoff:      time.Second,
		MaxBackoff:        30 * time.Second,
		BackoffMultiplier: 2.0,
	}
}

// ExponentialBackoff returns baseDelay * multiplier^attempt, capped at maxDelay.
func ExponentialBackoff(attempt int, baseDelay time.Duration, maxDelay time.Duration, multiplier float64) time.Duration {
	delay := time.Duration(float64(baseDelay) * math.Pow(multiplier, float64(attempt)))
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

// Jitter spreads delay by ±jitterPercent of its value to avoid synchronized
// retry waves (thundering herd). jitterPercent must be in (0,1); values outside
// that range return delay unchanged.
func Jitter(delay time.Duration, jitterPercent float64) time.Duration {
	if jitterPercent <= 0 || jitterPercent >= 1 {
		return delay
	}

	jitter := time.Duration(float64(delay) * jitterPercent * (2*rand.Float64() - 1))
	return delay + jitter
}
