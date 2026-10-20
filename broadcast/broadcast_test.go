package broadcast_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/broadcast"
	"github.com/rocketinsidecorp/forge-go/turbo"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func mustGlobal(t testing.TB, name string) broadcast.Channel {
	t.Helper()
	channel, err := broadcast.Global(name)
	if err != nil {
		t.Fatal(err)
	}
	return channel
}

func mustTenant(t testing.TB, tenant uuid.UUID, name string) broadcast.Channel {
	t.Helper()
	channel, err := broadcast.ForTenant(tenant, name)
	if err != nil {
		t.Fatal(err)
	}
	return channel
}

func TestChannels(t *testing.T) {
	t.Parallel()
	tenant := uuid.MustNew()
	if got := mustGlobal(t, "posts").Key(); got != "global/posts" {
		t.Fatalf("global key = %q", got)
	}
	scoped := mustTenant(t, tenant, "posts")
	if scoped.Key() != tenant.String()+"/posts" || scoped.IsGlobal() {
		t.Fatalf("tenant key = %q", scoped.Key())
	}
	if scoped.Key() == mustGlobal(t, "posts").Key() {
		t.Fatal("a tenant channel and the global channel of the same name must differ")
	}
	for _, channel := range []broadcast.Channel{mustGlobal(t, "a.b:c_d-e"), scoped} {
		parsed, err := broadcast.ParseKey(channel.Key())
		if err != nil || parsed != channel {
			t.Errorf("ParseKey(%q) = %v, %v", channel.Key(), parsed, err)
		}
	}
	for _, name := range []string{"", "Posts", "a b", "a/b", "-a", "_a", "a\n", "é", strings.Repeat("a", broadcast.MaxNameBytes+1)} {
		if _, err := broadcast.Global(name); err == nil {
			t.Errorf("name %q accepted", name)
		}
		if _, err := broadcast.ForTenant(tenant, name); err == nil {
			t.Errorf("tenant name %q accepted", name)
		}
	}
	if _, err := broadcast.ForTenant(uuid.UUID{}, "posts"); err == nil {
		t.Error("zero tenant accepted: it would silently be a global channel")
	}
	if _, err := broadcast.ForTenant(mustV4(t), "posts"); err == nil {
		t.Error("non-v7 tenant accepted")
	}
	for _, key := range []string{"", "posts", "global/", "global/Posts", "nope/posts", tenant.String() + "/", "/posts"} {
		if _, err := broadcast.ParseKey(key); err == nil {
			t.Errorf("ParseKey(%q) accepted", key)
		}
	}
}

func mustV4(t testing.TB) uuid.UUID {
	t.Helper()
	id := uuid.MustNew()
	id[6] = 0x40 | id[6]&0x0f
	return id
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newSigner(t testing.TB, mutate func(*broadcast.SignerOptions)) (*broadcast.Signer, *clock) {
	t.Helper()
	now := &clock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	opts := broadcast.SignerOptions{Key: testKey, Now: now.Now}
	if mutate != nil {
		mutate(&opts)
	}
	signer, err := broadcast.NewSigner(opts)
	if err != nil {
		t.Fatal(err)
	}
	return signer, now
}

func TestSignedNamesRoundTrip(t *testing.T) {
	t.Parallel()
	signer, _ := newSigner(t, nil)
	for _, channel := range []broadcast.Channel{mustGlobal(t, "posts"), mustTenant(t, uuid.MustNew(), "posts")} {
		token, err := signer.Sign(channel)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(token, " +/=&?#%") {
			t.Fatalf("token %q is not URL-safe", token)
		}
		got, err := signer.Verify(token)
		if err != nil || got != channel {
			t.Fatalf("Verify = %v, %v; want %v", got, err, channel)
		}
	}
	url, err := signer.SourceURL("/streams", mustGlobal(t, "posts"))
	if err != nil || !strings.HasPrefix(url, "/streams?stream=v1.") {
		t.Fatalf("SourceURL = %q, %v", url, err)
	}
}

func TestSignedNamesRejectTampering(t *testing.T) {
	t.Parallel()
	signer, _ := newSigner(t, nil)
	other, _ := newSigner(t, func(o *broadcast.SignerOptions) { o.Key = []byte("ffffffffffffffffffffffffffffffff") })
	tenantA, tenantB := uuid.MustNew(), uuid.MustNew()
	token, _ := signer.Sign(mustTenant(t, tenantA, "posts"))
	forgedClaims, _ := signer.Sign(mustTenant(t, tenantB, "posts"))

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token = %q", token)
	}
	foreign, _ := other.Sign(mustTenant(t, tenantA, "posts"))
	// Another tenant's payload under this tenant's signature.
	swapped := strings.Split(forgedClaims, ".")[0] + "." + strings.Split(forgedClaims, ".")[1] + "." + parts[2]
	flipped := []byte(token)
	flipped[len(flipped)-3] ^= 0x01

	for name, bad := range map[string]string{
		"empty":             "",
		"garbage":           "garbage",
		"no signature":      parts[0] + "." + parts[1],
		"empty signature":   parts[0] + "." + parts[1] + ".",
		"another key":       foreign,
		"swapped payload":   swapped,
		"flipped bit":       string(flipped),
		"wrong version":     "v2." + parts[1] + "." + parts[2],
		"unsigned (alg=no)": parts[0] + "." + parts[1] + ".AAAA",
		"too long":          token + strings.Repeat("A", 600),
		"padded signature":  token + "=",
	} {
		if _, err := signer.Verify(bad); !errors.Is(err, broadcast.ErrInvalidToken) {
			t.Errorf("%s: Verify = %v, want ErrInvalidToken", name, err)
		}
	}
}

func TestSignedNamesExpire(t *testing.T) {
	t.Parallel()
	signer, now := newSigner(t, func(o *broadcast.SignerOptions) { o.TTL = time.Hour })
	token, _ := signer.Sign(mustGlobal(t, "posts"))
	now.Advance(time.Hour - time.Second)
	if _, err := signer.Verify(token); err != nil {
		t.Fatalf("token should still be valid: %v", err)
	}
	now.Advance(time.Second)
	if _, err := signer.Verify(token); !errors.Is(err, broadcast.ErrExpiredToken) {
		t.Fatalf("expired token: %v", err)
	}
	defaults, clk := newSigner(t, nil)
	token, _ = defaults.Sign(mustGlobal(t, "posts"))
	clk.Advance(broadcast.DefaultTTL + time.Second)
	if _, err := defaults.Verify(token); !errors.Is(err, broadcast.ErrExpiredToken) {
		t.Fatalf("default TTL: %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	t.Parallel()
	oldKey := []byte("oldoldoldoldoldoldoldoldoldoldol")
	oldSigner, _ := newSigner(t, func(o *broadcast.SignerOptions) { o.Key = oldKey })
	token, _ := oldSigner.Sign(mustGlobal(t, "posts"))
	rotated, _ := newSigner(t, func(o *broadcast.SignerOptions) { o.PreviousKeys = [][]byte{oldKey} })
	if _, err := rotated.Verify(token); err != nil {
		t.Fatalf("a previous key must still verify: %v", err)
	}
	fresh, _ := rotated.Sign(mustGlobal(t, "posts"))
	if _, err := oldSigner.Verify(fresh); err == nil {
		t.Fatal("tokens must be signed with the current key only")
	}
	unrotated, _ := newSigner(t, nil)
	if _, err := unrotated.Verify(token); err == nil {
		t.Fatal("a retired key must not verify")
	}
}

func TestSignerValidation(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]broadcast.SignerOptions{
		"no key":       {},
		"short key":    {Key: []byte("short")},
		"short prev":   {Key: testKey, PreviousKeys: [][]byte{[]byte("short")}},
		"negative ttl": {Key: testKey, TTL: -time.Second},
		"huge ttl":     {Key: testKey, TTL: broadcast.MaxTTL + time.Second},
	} {
		if _, err := broadcast.NewSigner(opts); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	signer, _ := newSigner(t, nil)
	if _, err := signer.Sign(broadcast.Channel{Name: "Bad Name"}); err == nil {
		t.Error("an invalid channel was signed")
	}
	if _, err := signer.Sign(broadcast.Channel{Tenant: uuid.UUID{1}, Name: "posts"}); err == nil {
		t.Error("a non-v7 tenant was signed")
	}
	if _, err := broadcast.NewSignerFromBase64("%%%", broadcast.SignerOptions{}); err == nil || strings.Contains(err.Error(), "%%%") {
		t.Errorf("bad base64 key: %v", err)
	}
	for _, encoded := range []string{
		"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
		"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY",
	} {
		if _, err := broadcast.NewSignerFromBase64(encoded, broadcast.SignerOptions{}); err != nil {
			t.Errorf("%s: %v", encoded, err)
		}
	}
}

func TestPublishRendersStreams(t *testing.T) {
	t.Parallel()
	broker, err := broadcast.NewMemory(context.Background(), broadcast.MemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	channel := mustGlobal(t, "posts")
	sub, err := broker.Subscribe(context.Background(), channel)
	if err != nil {
		t.Fatal(err)
	}
	remove, _ := turbo.Remove("post_1")
	if err := broadcast.Publish(context.Background(), broker, channel, remove); err != nil {
		t.Fatal(err)
	}
	if got := (<-sub.Messages()).Data; got != `<turbo-stream action="remove" target="post_1"></turbo-stream>`+"\n" {
		t.Fatalf("message = %q", got)
	}
	if err := broadcast.Publish(context.Background(), broker, channel); err == nil {
		t.Error("publishing nothing succeeded")
	}
	if err := broadcast.Publish(context.Background(), nil, channel, remove); err == nil {
		t.Error("a nil broker was accepted")
	}
	if err := broadcast.Publish(context.Background(), broker, broadcast.Channel{}, remove); err == nil {
		t.Error("the zero channel was accepted")
	}
}

func TestRandomKeysAreUsable(t *testing.T) {
	t.Parallel()
	first, err := broadcast.RandomKey()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := broadcast.RandomKey()
	if len(first) != broadcast.MinKeyBytes || string(first) == string(second) {
		t.Fatal("keys must be 32 bytes and differ")
	}
	if _, err := broadcast.NewSigner(broadcast.SignerOptions{Key: first}); err != nil {
		t.Fatal(err)
	}
}
