package realtime

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/fabriciobonjorno/forge-go/auth"
	"github.com/fabriciobonjorno/forge-go/uuid"
)

var (
	staticSegmentPattern = regexp.MustCompile(`^[a-z0-9_.-]+$`)
	paramNamePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// Params holds the values captured by the {name} segments of a Channel name.
type Params map[string]string

// Channel declares a family of topics clients may subscribe to. A topic no
// Channel matches cannot be subscribed to: access is denied by default.
type Channel struct {
	// Name is an exact topic ("orders") or a pattern whose colon-separated
	// segments may be {param} captures ("room:{id}"). A topic matches when it
	// has the same number of segments and every static segment is equal.
	// When several channels match, the one with the most static segments wins;
	// two channels with the same shape ("room:{id}" and "room:{room}") are
	// rejected as ambiguous.
	Name string
	// Permission is the permission the authenticated principal must hold.
	// It is required: a channel without one is rejected, not left open.
	Permission auth.Permission
	// Global makes the topic shared by all tenants. The default (false) scopes
	// every subscription to the connection's tenant. Subscribers still need
	// Permission, and publishers must opt in with the Global publish option.
	Global bool
	// Authorize is an optional additional check, for example that the room
	// belongs to the principal's tenant. It runs on every subscribe, after the
	// permission check, and any error denies the subscription. It must be
	// fast and must not retain params.
	Authorize func(ctx context.Context, principal auth.Principal, params Params) error
}

type channel struct {
	Channel
	segments []string
	static   int
	shape    string
}

func compileChannel(c Channel) (*channel, error) {
	if c.Permission == "" {
		return nil, fmt.Errorf("realtime channel %q requires a permission", c.Name)
	}
	if _, err := auth.NewPermission(string(c.Permission)); err != nil {
		return nil, fmt.Errorf("realtime channel %q: %w", c.Name, err)
	}
	compiled := &channel{Channel: c, segments: strings.Split(c.Name, ":")}
	seen := map[string]bool{}
	shape := make([]string, len(compiled.segments))
	for i, segment := range compiled.segments {
		if strings.HasPrefix(segment, "{") || strings.HasSuffix(segment, "}") {
			name, ok := strings.CutPrefix(segment, "{")
			if ok {
				name, ok = strings.CutSuffix(name, "}")
			}
			if !ok || !paramNamePattern.MatchString(name) {
				return nil, fmt.Errorf("realtime channel %q has an invalid parameter segment %q", c.Name, segment)
			}
			if seen[name] {
				return nil, fmt.Errorf("realtime channel %q repeats parameter %q", c.Name, name)
			}
			seen[name] = true
			shape[i] = "{}"
			continue
		}
		if !staticSegmentPattern.MatchString(segment) {
			return nil, fmt.Errorf("realtime channel %q has an invalid segment %q", c.Name, segment)
		}
		compiled.static++
		shape[i] = segment
	}
	compiled.shape = strings.Join(shape, ":")
	// Every topic the channel can match must itself be a valid topic name.
	probe := strings.ReplaceAll(compiled.shape, "{}", "p")
	if len(c.Name) > 128 || !topicPattern.MatchString(probe) {
		return nil, fmt.Errorf("realtime channel name %q must match %s with optional {param} segments", c.Name, topicPattern)
	}
	return compiled, nil
}

// match reports whether topic belongs to the channel and returns the captured
// parameters. topic must already be a valid topic name.
func (c *channel) match(topic string) (Params, bool) {
	parts := strings.Split(topic, ":")
	if len(parts) != len(c.segments) {
		return nil, false
	}
	var params Params
	for i, segment := range c.segments {
		if strings.HasPrefix(segment, "{") {
			if !staticSegmentPattern.MatchString(parts[i]) {
				return nil, false
			}
			if params == nil {
				params = Params{}
			}
			params[segment[1:len(segment)-1]] = parts[i]
			continue
		}
		if parts[i] != segment {
			return nil, false
		}
	}
	return params, true
}

// Register declares channels. Names must be unique and unambiguous.
func (b *Broker) Register(channels ...Channel) error {
	b.chMu.Lock()
	defer b.chMu.Unlock()
	next := append([]*channel(nil), b.channels...)
	for _, declared := range channels {
		compiled, err := compileChannel(declared)
		if err != nil {
			return err
		}
		for _, existing := range next {
			if existing.shape == compiled.shape {
				return fmt.Errorf("realtime channel %q is ambiguous with %q", compiled.Name, existing.Name)
			}
		}
		next = append(next, compiled)
	}
	b.channels = next
	return nil
}

// lookup returns the most specific channel for topic.
func (b *Broker) lookup(topic string) (*channel, Params) {
	b.chMu.RLock()
	defer b.chMu.RUnlock()
	var best *channel
	var bestParams Params
	for _, candidate := range b.channels {
		params, ok := candidate.match(topic)
		if ok && (best == nil || candidate.static > best.static) {
			best, bestParams = candidate, params
		}
	}
	return best, bestParams
}

// authorizeSubscription decides whether the connection's principal may
// subscribe to topic and returns the scope the subscription lives in. Every
// refusal is ErrSubscriptionDenied.
func (c *Conn) authorizeSubscription(ctx context.Context, topic string) (scope, error) {
	channel, params := c.broker.lookup(topic)
	if channel == nil || !c.principal.Can(channel.Permission) {
		return scope{}, ErrSubscriptionDenied
	}
	if channel.Authorize != nil {
		if err := channel.Authorize(ctx, c.principal, params); err != nil {
			return scope{}, ErrSubscriptionDenied.WithCause(err)
		}
	}
	if channel.Global {
		return scope{global: true}, nil
	}
	return tenantScope(c.principal.Tenant()), nil
}

var errNilContext = errors.New("realtime: context is required")

// floorAt is the smallest UUIDv7 for the instant t; every ID generated before
// t compares lower.
func floorAt(t time.Time) uuid.UUID {
	var id uuid.UUID
	putMillis(&id, t)
	id[6] = 0x70
	id[8] = 0x80
	return id
}

// ceilingAt is the largest UUIDv7 for the instant t; every ID generated at or
// before t compares lower or equal.
func ceilingAt(t time.Time) uuid.UUID {
	var id uuid.UUID
	putMillis(&id, t)
	id[6] = 0x7f
	id[7] = 0xff
	id[8] = 0xbf
	for i := 9; i < 16; i++ {
		id[i] = 0xff
	}
	return id
}

func putMillis(id *uuid.UUID, t time.Time) {
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], uint64(t.UnixMilli())<<16) // #nosec G115 -- UnixMilli is positive for any realistic clock
	copy(id[:6], timestamp[:6])
}
