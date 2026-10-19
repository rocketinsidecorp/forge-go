// Package proxy implements forge-proxy, the reverse proxy that `forge deploy`
// runs on every web host (ADR 0030). It routes requests by Host to the
// current target container of each service and switches targets without
// dropping requests: a new target receives traffic only after its health
// check passed several times in a row, and the previous target is drained
// before the deploy command returns.
//
// The proxy is controlled through a Unix socket inside its container (see
// [ListenControl]); it has no network-reachable control surface.
package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration encoded as a Go duration string ("1.5s") in
// JSON, so control messages and the state file stay readable.
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler. Only strings are accepted.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return errors.New("duration must be a string such as \"5s\"")
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("invalid duration %q", text)
	}
	*d = Duration(parsed)
	return nil
}

// Defaults applied by [DeployRequest.normalize] to zero fields.
const (
	DefaultHealthPath      = "/health/ready"
	DefaultHealthInterval  = time.Second
	DefaultHealthTimeout   = 2 * time.Second
	DefaultHealthyCount    = 3
	DefaultDeployTimeout   = 60 * time.Second
	DefaultDrainTimeout    = 30 * time.Second
	DefaultResponseTimeout = 30 * time.Second
	DefaultMaxBodyBytes    = 64 << 20
	DefaultBufferBytes     = 1 << 20
)

// DeployRequest registers or updates a service and switches it to Target.
type DeployRequest struct {
	// Service names the routing entry; one service per application.
	Service string `json:"service"`
	// Target is the host:port of the new container on the shared network.
	Target string `json:"target"`
	// Hosts are the request Host names routed to this service. A service
	// without hosts receives requests whose host matches no other service.
	Hosts []string `json:"hosts,omitempty"`
	// TLS obtains certificates for Hosts from the ACME CA and redirects
	// plain HTTP requests to HTTPS.
	TLS bool `json:"tls,omitempty"`

	HealthPath      string   `json:"health_path,omitempty"`
	HealthInterval  Duration `json:"health_interval,omitempty"`
	HealthTimeout   Duration `json:"health_timeout,omitempty"`
	HealthyCount    int      `json:"healthy_count,omitempty"`
	DeployTimeout   Duration `json:"deploy_timeout,omitempty"`
	DrainTimeout    Duration `json:"drain_timeout,omitempty"`
	ResponseTimeout Duration `json:"response_timeout,omitempty"`
	// MaxBodyBytes rejects larger request bodies with 413.
	MaxBodyBytes int64 `json:"max_body_bytes,omitempty"`
	// BufferBytes is the largest request body read completely before the
	// request is forwarded, so slow clients do not hold application
	// connections. Larger bodies (up to MaxBodyBytes) are streamed.
	BufferBytes int64 `json:"buffer_bytes,omitempty"`
}

// DeployResult reports a completed switch.
type DeployResult struct {
	Service  string `json:"service"`
	Target   string `json:"target"`
	Previous string `json:"previous,omitempty"`
	// Drained is false when requests were still in flight on the previous
	// target when DrainTimeout expired; they continue until the old
	// container is stopped.
	Drained         bool   `json:"drained"`
	InFlight        int64  `json:"in_flight_remaining"`
	HealthChecks    int    `json:"health_checks"`
	StateSaveFailed string `json:"state_save_failed,omitempty"`
}

var (
	serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	hostLabelPattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	targetHostPattern  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,252})$`)
)

// ValidServiceName reports whether name is an acceptable service name.
func ValidServiceName(name string) bool { return serviceNamePattern.MatchString(name) }

// ValidHost reports whether host is a lower-case DNS name acceptable for
// routing and certificate issuance. IP addresses and wildcards are refused.
func ValidHost(host string) bool {
	if len(host) == 0 || len(host) > 253 || net.ParseIP(host) != nil {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if !hostLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func validTarget(target string) error {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return errors.New("target must be host:port")
	}
	if !targetHostPattern.MatchString(host) {
		return errors.New("target host is invalid")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("target port must be 1-65535")
	}
	return nil
}

// normalize applies defaults and validates the request.
func (r *DeployRequest) normalize() error {
	if !ValidServiceName(r.Service) {
		return errors.New("service name must match ^[a-z0-9][a-z0-9_-]{0,62}$")
	}
	if err := validTarget(r.Target); err != nil {
		return err
	}
	seen := make(map[string]bool, len(r.Hosts))
	for i, host := range r.Hosts {
		host = strings.ToLower(host)
		if !ValidHost(host) {
			return fmt.Errorf("host %q is not a valid DNS name", host)
		}
		if seen[host] {
			return fmt.Errorf("host %q is listed twice", host)
		}
		seen[host] = true
		r.Hosts[i] = host
	}
	if r.TLS && len(r.Hosts) == 0 {
		return errors.New("tls requires at least one host")
	}
	if r.HealthPath == "" {
		r.HealthPath = DefaultHealthPath
	}
	if !strings.HasPrefix(r.HealthPath, "/") || strings.ContainsAny(r.HealthPath, " \t\r\n?#") {
		return errors.New("health path must be an absolute path without query")
	}
	defaults := []struct {
		value *Duration
		def   time.Duration
		max   time.Duration
		name  string
	}{
		{&r.HealthInterval, DefaultHealthInterval, time.Minute, "health interval"},
		{&r.HealthTimeout, DefaultHealthTimeout, time.Minute, "health timeout"},
		{&r.DeployTimeout, DefaultDeployTimeout, time.Hour, "deploy timeout"},
		{&r.DrainTimeout, DefaultDrainTimeout, time.Hour, "drain timeout"},
		{&r.ResponseTimeout, DefaultResponseTimeout, time.Hour, "response timeout"},
	}
	for _, d := range defaults {
		if *d.value == 0 {
			*d.value = Duration(d.def)
		}
		if *d.value < 0 || time.Duration(*d.value) > d.max {
			return fmt.Errorf("%s must be between 0 and %s", d.name, d.max)
		}
	}
	if r.HealthyCount == 0 {
		r.HealthyCount = DefaultHealthyCount
	}
	if r.HealthyCount < 1 || r.HealthyCount > 100 {
		return errors.New("healthy count must be between 1 and 100")
	}
	if r.MaxBodyBytes == 0 {
		r.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if r.BufferBytes == 0 {
		r.BufferBytes = DefaultBufferBytes
	}
	if r.MaxBodyBytes < 0 || r.BufferBytes < 0 {
		return errors.New("body limits must be positive")
	}
	r.BufferBytes = min(r.BufferBytes, r.MaxBodyBytes)
	return nil
}
