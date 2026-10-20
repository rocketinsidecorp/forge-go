// Package health provides liveness and readiness checks.
package health

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

type Check func(context.Context) error

type Status struct {
	Name    string        `json:"name"`
	Healthy bool          `json:"healthy"`
	Latency time.Duration `json:"-"`
}

type Result struct {
	Healthy bool     `json:"healthy"`
	Checks  []Status `json:"checks"`
}

// OpenAPIName names the types in OpenAPI documents (openapi.Namer).
func (Status) OpenAPIName() string { return "HealthCheck" }

// OpenAPIName names the types in OpenAPI documents (openapi.Namer).
func (Result) OpenAPIName() string { return "HealthReport" }

// DefaultCheckTimeout bounds each readiness check so a hung dependency (or a
// saturated connection pool) reports unready quickly instead of blocking the
// probe until the orchestrator gives up.
const DefaultCheckTimeout = 2 * time.Second

type Registry struct {
	mu      sync.RWMutex
	checks  map[string]Check
	timeout time.Duration
}

func New() *Registry { return NewWithTimeout(DefaultCheckTimeout) }

// NewWithTimeout is New with a custom per-check timeout.
func NewWithTimeout(timeout time.Duration) *Registry {
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	return &Registry{checks: make(map[string]Check), timeout: timeout}
}

func (r *Registry) Register(name string, check Check) error {
	if name == "" || check == nil {
		return errors.New("health check name and function are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.checks[name]; exists {
		return errors.New("health check already registered")
	}
	r.checks[name] = check
	return nil
}

func (r *Registry) Ready(ctx context.Context) Result {
	r.mu.RLock()
	names := make([]string, 0, len(r.checks))
	checks := make(map[string]Check, len(r.checks))
	for name, check := range r.checks {
		names = append(names, name)
		checks[name] = check
	}
	r.mu.RUnlock()
	sort.Strings(names)

	result := Result{Healthy: true, Checks: make([]Status, 0, len(names))}
	for _, name := range names {
		started := time.Now()
		checkCtx, cancel := context.WithTimeout(ctx, r.timeout)
		err := checks[name](checkCtx)
		cancel()
		result.Checks = append(result.Checks, Status{Name: name, Healthy: err == nil, Latency: time.Since(started)})
		if err != nil {
			result.Healthy = false
		}
	}
	return result
}
