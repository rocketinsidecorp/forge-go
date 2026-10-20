// Package broadcast pushes Turbo Streams to browsers in real time over
// Server-Sent Events: Rails' Turbo::StreamsChannel / Action Cable for the
// common case, without WebSockets.
//
//	broker, _ := broadcast.NewMemory(ctx, broadcast.MemoryOptions{}) // or broadcast/postgres for several replicas
//	signer, _ := broadcast.NewSigner(broadcast.SignerOptions{Key: key})
//	events, _ := broadcast.NewHandler(broadcast.HandlerOptions{Broker: broker, Signer: signer})
//	app.Handle("GET /streams", authenticated(events))
//
// A page subscribes with {{turboStreamFrom "posts"}}, which renders a
// <turbo-stream-source> whose URL carries a signed channel name; server code
// publishes with Publish. Clients can only subscribe to channels the server
// rendered for them: the name is an HMAC-signed, expiring capability, and a
// tenant-scoped channel additionally requires the caller to be in that
// tenant. Delivery is best effort and at most once: there is no replay, a
// subscriber that cannot keep up is dropped (the browser reconnects), and
// messages published while a client is disconnected are not delivered.
package broadcast

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// MaxNameBytes is the longest channel name.
const MaxNameBytes = 96

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,95}$`)

// ErrInvalidChannel is returned for a malformed channel name or tenant.
var ErrInvalidChannel = errors.New("broadcast: channel names are 1-96 bytes of [a-z0-9_.:-] starting with a letter or digit")

// Channel names a stream of messages. A tenant-scoped channel belongs to one
// tenant; only callers of that tenant may subscribe, and a message published
// to it never reaches another tenant's subscribers. The zero Tenant means a
// global channel, which is for content every authorized viewer may see.
type Channel struct {
	Tenant uuid.UUID
	Name   string
}

// Global returns a channel not tied to a tenant. Use it only for content that
// is the same for every tenant (for example an announcement); tenant data
// belongs on ForTenant channels.
func Global(name string) (Channel, error) {
	if !namePattern.MatchString(name) {
		return Channel{}, ErrInvalidChannel
	}
	return Channel{Name: name}, nil
}

// ForTenant returns a channel owned by tenant, which must be a UUIDv7 as
// everywhere else in Forge.
func ForTenant(tenant uuid.UUID, name string) (Channel, error) {
	if _, err := tenancy.New(tenant); err != nil {
		return Channel{}, fmt.Errorf("broadcast: %w", err)
	}
	if !namePattern.MatchString(name) {
		return Channel{}, ErrInvalidChannel
	}
	return Channel{Tenant: tenant, Name: name}, nil
}

// IsGlobal reports whether the channel is not tenant-scoped.
func (c Channel) IsGlobal() bool { return c.Tenant == (uuid.UUID{}) }

// Key is the canonical text form, "global/NAME" or "TENANT-UUID/NAME". Names
// never contain '/', so keys are unambiguous.
func (c Channel) Key() string {
	if c.IsGlobal() {
		return "global/" + c.Name
	}
	return c.Tenant.String() + "/" + c.Name
}

// valid re-checks a Channel that may have been built by hand.
func (c Channel) valid() error {
	if !namePattern.MatchString(c.Name) {
		return ErrInvalidChannel
	}
	if !c.IsGlobal() {
		if _, err := tenancy.New(c.Tenant); err != nil {
			return fmt.Errorf("broadcast: %w", err)
		}
	}
	return nil
}

// ParseKey is the inverse of Key.
func ParseKey(key string) (Channel, error) {
	scope, name, ok := strings.Cut(key, "/")
	if !ok {
		return Channel{}, ErrInvalidChannel
	}
	if scope == "global" {
		return Global(name)
	}
	tenant, err := uuid.Parse(scope)
	if err != nil {
		return Channel{}, ErrInvalidChannel
	}
	return ForTenant(tenant, name)
}
