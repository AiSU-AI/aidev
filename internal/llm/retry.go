package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// RetryConfig defines retry behavior for LLM calls
type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Backoff     BackoffType
}

// BackoffType determines how delays increase between retries
type BackoffType string

const (
	BackoffLinear    BackoffType = "linear"
	BackoffExponential BackoffType = "exponential"
	BackoffFibonacci BackoffType = "fibonacci"
)

// DefaultRetryConfig returns sensible defaults for LLM operations
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Second,
		MaxDelay:    30 * time.Second,
		Backoff:     BackoffExponential,
	}
}

// RetryableError determines if an error should trigger a retry
type RetryableError interface {
	error
	IsRetryable() bool
}

// Retryable wraps an error to mark it as retryable
type Retryable struct {
	Err error
}

func (r Retryable) Error() string {
	return r.Err.Error()
}

func (r Retryable) IsRetryable() bool {
	return true
}

func (r Retryable) Unwrap() error {
	return r.Err
}

// NewRetryable creates a new retryable error
func NewRetryable(err error) Retryable {
	return Retryable{Err: err}
}

// NonRetryable wraps an error to mark it as non-retryable
type NonRetryable struct {
	Err error
}

func (n NonRetryable) Error() string {
	return n.Err.Error()
}

func (n NonRetryable) IsRetryable() bool {
	return false
}

func (n NonRetryable) Unwrap() error {
	return n.Err
}

// NewNonRetryable creates a new non-retryable error
func NewNonRetryable(err error) NonRetryable {
	return NonRetryable{Err: err}
}

// RetryWrapper adds retry logic to any Provider
type RetryWrapper struct {
	provider Provider
	config   RetryConfig
}

// NewRetryWrapper creates a new provider with retry logic
func NewRetryWrapper(provider Provider, config RetryConfig) *RetryWrapper {
	return &RetryWrapper{
		provider: provider,
		config:   config,
	}
}

// Name returns the wrapped provider's name
func (r *RetryWrapper) Name() string {
	return r.provider.Name() + " (with retry)"
}

// Complete implements Provider with retry logic
func (r *RetryWrapper) Complete(ctx context.Context, req Request) (Response, error) {
	var lastErr error
	
	for attempt := 1; attempt <= r.config.MaxAttempts; attempt++ {
		resp, err := r.provider.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}

		lastErr = err

		// Check if error is retryable
		if !isRetryable(err) {
			break
		}

		// Don't retry on last attempt
		if attempt == r.config.MaxAttempts {
			break
		}

		// Calculate delay for next attempt
		delay := r.calculateDelay(attempt)
		
		// Log retry attempt (could be enhanced with proper logging)
		fmt.Printf("Attempt %d/%d failed for %s: %v. Retrying in %v...\n", 
			attempt, r.config.MaxAttempts, r.provider.Name(), err, delay)

		// Wait before retry (with context cancellation support)
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(delay):
			// Continue to next attempt
		}
	}

	return Response{}, fmt.Errorf("after %d attempts, last error: %w", r.config.MaxAttempts, lastErr)
}

// CompleteWithTools forwards a tool-aware request to the wrapped
// provider if it implements ToolAwareProvider, applying the same retry
// loop that Complete uses. If the inner provider does not support
// native tool use, it returns ErrToolsNotSupported so callers can fall
// back to the legacy Complete path.
func (r *RetryWrapper) CompleteWithTools(ctx context.Context, req ToolAwareRequest) (ToolAwareResponse, error) {
	inner, ok := r.provider.(ToolAwareProvider)
	if !ok {
		return ToolAwareResponse{}, ErrToolsNotSupported
	}

	var lastErr error
	for attempt := 1; attempt <= r.config.MaxAttempts; attempt++ {
		resp, err := inner.CompleteWithTools(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		// ErrToolsNotSupported is a capability signal, not a
		// transport failure. Surface it unwrapped so the Implementer's
		// errors.Is check at the top of Run() can route around it.
		if errors.Is(err, ErrToolsNotSupported) {
			return ToolAwareResponse{}, err
		}
		if !isRetryable(err) {
			break
		}
		if attempt == r.config.MaxAttempts {
			break
		}
		delay := r.calculateDelay(attempt)
		fmt.Printf("Attempt %d/%d failed for %s (tools): %v. Retrying in %v...\n",
			attempt, r.config.MaxAttempts, r.provider.Name(), err, delay)
		select {
		case <-ctx.Done():
			return ToolAwareResponse{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	return ToolAwareResponse{}, fmt.Errorf("after %d attempts, last error: %w", r.config.MaxAttempts, lastErr)
}

// calculateDelay computes the delay for a given attempt based on backoff strategy
func (r *RetryWrapper) calculateDelay(attempt int) time.Duration {
	var delay time.Duration

	switch r.config.Backoff {
	case BackoffLinear:
		delay = time.Duration(attempt) * r.config.BaseDelay
	case BackoffExponential:
		delay = r.config.BaseDelay * time.Duration(math.Pow(2, float64(attempt-1)))
	case BackoffFibonacci:
		delay = r.config.BaseDelay * time.Duration(fibonacci(attempt))
	default:
		delay = r.config.BaseDelay
	}

	// Cap at max delay
	if delay > r.config.MaxDelay {
		delay = r.config.MaxDelay
	}

	return delay
}

// fibonacci returns the nth Fibonacci number (starting with F(1) = 1, F(2) = 1)
func fibonacci(n int) int {
	if n <= 2 {
		return 1
	}
	
	a, b := 1, 1
	for i := 3; i <= n; i++ {
		a, b = b, a+b
	}
	return b
}

// isRetryable determines if an error should trigger a retry
func isRetryable(err error) bool {
	// Check if error implements RetryableError
	if retryable, ok := err.(RetryableError); ok {
		return retryable.IsRetryable()
	}

	// Check for specific error patterns that are typically retryable
	errStr := err.Error()
	
	// Network-related errors
	retryablePatterns := []string{
		"connection refused",
		"connection reset",
		"timeout",
		"temporary failure",
		"service unavailable",
		"rate limit",
		"too many requests",
		"internal server error",
		"bad gateway",
		"gateway timeout",
		"network unreachable",
		"no such host",
		"connection timed out",
		"read: connection reset",
		"write: broken pipe",
	}

	for _, pattern := range retryablePatterns {
		if contains(errStr, pattern) {
			return true
		}
	}

	// Non-retryable patterns
	nonRetryablePatterns := []string{
		"unauthorized",
		"forbidden",
		"not found",
		"invalid request",
		"malformed",
		"bad request",
		"authentication failed",
		"permission denied",
		"quota exceeded",
	}

	for _, pattern := range nonRetryablePatterns {
		if contains(errStr, pattern) {
			return false
		}
	}

	// Default to retryable for unknown errors
	return true
}

// contains checks if a string contains a substring (case-insensitive)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && 
		   (s == substr || 
		    len(s) > len(substr) && 
		    (s[:len(substr)] == substr || 
		     s[len(s)-len(substr):] == substr ||
		     containsMiddle(s, substr)))
}

func containsMiddle(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// CircuitBreaker implements the circuit breaker pattern for providers
type CircuitBreaker struct {
	provider     Provider
	maxFailures  int
	resetTimeout time.Duration
	failures     int
	lastFailTime time.Time
	state        CircuitState
}

type CircuitState int

const (
	StateClosed CircuitState = iota
	StateOpen
	StateHalfOpen
)

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(provider Provider, maxFailures int, resetTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		provider:     provider,
		maxFailures:  maxFailures,
		resetTimeout: resetTimeout,
		state:        StateClosed,
	}
}

// Name returns the provider name with circuit breaker status
func (cb *CircuitBreaker) Name() string {
	status := ""
	switch cb.state {
	case StateClosed:
		status = "[closed]"
	case StateOpen:
		status = "[open]"
	case StateHalfOpen:
		status = "[half-open]"
	}
	return cb.provider.Name() + " " + status
}

// Complete implements Provider with circuit breaker logic
func (cb *CircuitBreaker) Complete(ctx context.Context, req Request) (Response, error) {
	// Check circuit state
	if cb.state == StateOpen {
		if time.Since(cb.lastFailTime) > cb.resetTimeout {
			cb.state = StateHalfOpen
		} else {
			return Response{}, fmt.Errorf("circuit breaker is open for %s", cb.provider.Name())
		}
	}

	resp, err := cb.provider.Complete(ctx, req)
	if err != nil {
		cb.onFailure()
		return resp, err
	}

	cb.onSuccess()
	return resp, nil
}

// CompleteWithTools forwards a tool-aware request to the inner provider
// if it implements ToolAwareProvider, applying the same circuit-breaker
// state transitions as Complete. Returns ErrToolsNotSupported when the
// inner provider lacks native tool use.
func (cb *CircuitBreaker) CompleteWithTools(ctx context.Context, req ToolAwareRequest) (ToolAwareResponse, error) {
	inner, ok := cb.provider.(ToolAwareProvider)
	if !ok {
		return ToolAwareResponse{}, ErrToolsNotSupported
	}

	if cb.state == StateOpen {
		if time.Since(cb.lastFailTime) > cb.resetTimeout {
			cb.state = StateHalfOpen
		} else {
			return ToolAwareResponse{}, fmt.Errorf("circuit breaker is open for %s", cb.provider.Name())
		}
	}

	resp, err := inner.CompleteWithTools(ctx, req)
	if err != nil {
		// ErrToolsNotSupported is a capability signal, not a
		// transport failure — do not count it against the breaker.
		if errors.Is(err, ErrToolsNotSupported) {
			return resp, err
		}
		cb.onFailure()
		return resp, err
	}
	cb.onSuccess()
	return resp, nil
}

// onFailure handles a failed call
func (cb *CircuitBreaker) onFailure() {
	cb.failures++
	cb.lastFailTime = time.Now()

	if cb.failures >= cb.maxFailures {
		cb.state = StateOpen
	}
}

// onSuccess handles a successful call
func (cb *CircuitBreaker) onSuccess() {
	cb.failures = 0
	cb.state = StateClosed
}

// ProviderWithMiddleware allows chaining multiple middleware
func ProviderWithMiddleware(provider Provider, middlewares ...func(Provider) Provider) Provider {
	result := provider
	for _, middleware := range middlewares {
		result = middleware(result)
	}
	return result
}

// WithRetry adds retry middleware
func WithRetry(config RetryConfig) func(Provider) Provider {
	return func(provider Provider) Provider {
		return NewRetryWrapper(provider, config)
	}
}

// WithCircuitBreaker adds circuit breaker middleware
func WithCircuitBreaker(maxFailures int, resetTimeout time.Duration) func(Provider) Provider {
	return func(provider Provider) Provider {
		return NewCircuitBreaker(provider, maxFailures, resetTimeout)
	}
}
