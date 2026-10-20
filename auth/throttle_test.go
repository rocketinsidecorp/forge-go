package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestMemoryLoginThrottlerLimitsAccountAndResetsOnSuccess(t *testing.T) {
	now := time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)
	limiter, err := newMemoryLoginThrottler(LoginThrottleConfig{
		Window:       time.Minute,
		AccountLimit: 2,
		SourceLimit:  10,
		MaxEntries:   100,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	key := deriveLoginThrottleKey("alice@example.com", "192.0.2.10")
	for i := 0; i < 2; i++ {
		decision, err := limiter.Attempt(context.Background(), key)
		if err != nil || !decision.Allowed {
			t.Fatalf("attempt %d decision=%+v err=%v", i+1, decision, err)
		}
	}
	decision, err := limiter.Attempt(context.Background(), key)
	if err != nil || decision.Allowed || decision.RetryAfter != time.Minute {
		t.Fatalf("throttled decision=%+v err=%v", decision, err)
	}
	if err := limiter.Success(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	decision, err = limiter.Attempt(context.Background(), key)
	if err != nil || !decision.Allowed {
		t.Fatalf("after success decision=%+v err=%v", decision, err)
	}
}

func TestMemoryLoginThrottlerLimitsSourceAcrossAccounts(t *testing.T) {
	limiter, err := newMemoryLoginThrottler(LoginThrottleConfig{
		Window:       time.Minute,
		AccountLimit: 10,
		SourceLimit:  2,
		MaxEntries:   100,
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	keys := []LoginThrottleKey{
		deriveLoginThrottleKey("a@example.com", "192.0.2.10"),
		deriveLoginThrottleKey("b@example.com", "192.0.2.10"),
		deriveLoginThrottleKey("c@example.com", "192.0.2.10"),
	}
	for i := 0; i < 2; i++ {
		if decision, err := limiter.Attempt(context.Background(), keys[i]); err != nil || !decision.Allowed {
			t.Fatalf("attempt %d decision=%+v err=%v", i+1, decision, err)
		}
	}
	if decision, err := limiter.Attempt(context.Background(), keys[2]); err != nil || decision.Allowed || decision.RetryAfter <= 0 {
		t.Fatalf("source limit decision=%+v err=%v", decision, err)
	}
}

func TestLoginServiceThrottlesBeforeCredentialLookup(t *testing.T) {
	limiter, err := newMemoryLoginThrottler(LoginThrottleConfig{
		Window:       time.Minute,
		AccountLimit: 1,
		SourceLimit:  10,
		MaxEntries:   100,
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	store := &loginStoreStub{}
	service, err := NewLoginService(store, &sessionStub{}, WithLoginThrottler(limiter))
	if err != nil {
		t.Fatal(err)
	}
	input := LoginInput{Email: "a@example.com", Password: "wrong", TenantSlug: "acme", Source: "192.0.2.10"}
	if _, err := service.Login(context.Background(), input); !errors.Is(err, ErrCredentialsInvalid) {
		t.Fatalf("first error=%v", err)
	}
	store.lookupErr = errors.New("lookup should not run again")
	_, err = service.Login(context.Background(), input)
	var throttled *LoginThrottledError
	if !errors.As(err, &throttled) || !errors.Is(err, ErrLoginThrottled) || throttled.RetryAfter <= 0 {
		t.Fatalf("second error=%v", err)
	}
}

// fakeThrottleClock is a settable clock for throttle tests.
type fakeThrottleClock struct{ now time.Time }

func (c *fakeThrottleClock) Now() time.Time { return c.now }

func (c *fakeThrottleClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestThrottler(t *testing.T, config LoginThrottleConfig) (*MemoryLoginThrottler, *fakeThrottleClock) {
	t.Helper()
	clock := &fakeThrottleClock{now: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
	limiter, err := newMemoryLoginThrottler(config, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return limiter, clock
}

func testThrottleConfig() LoginThrottleConfig {
	return LoginThrottleConfig{Window: time.Minute, PairLimit: 2, AccountLimit: 4, SourceLimit: 6, MaxEntries: 30}
}

func allowed(t *testing.T, limiter LoginThrottler, key LoginThrottleKey) bool {
	t.Helper()
	decision, err := limiter.Attempt(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return decision.Allowed
}

func TestOneSourceCannotLockALoginOutForOtherSources(t *testing.T) {
	limiter, _ := newTestThrottler(t, testThrottleConfig())
	attacker := deriveLoginThrottleKey("owner@example.com", "192.0.2.66")
	for i := 0; i < 2; i++ {
		if !allowed(t, limiter, attacker) {
			t.Fatalf("attacker attempt %d refused too early", i+1)
		}
	}
	// The attacker keeps hammering: refused attempts must not count against
	// the login's shared budget, or one source would lock the owner out.
	for i := 0; i < 100; i++ {
		if allowed(t, limiter, attacker) {
			t.Fatal("the attacker's pair limit did not hold")
		}
	}
	if !allowed(t, limiter, deriveLoginThrottleKey("owner@example.com", "192.0.2.1")) {
		t.Fatal("the owner, from another source, was locked out by one source")
	}
}

func TestGuessingOneLoginFromManySourcesIsCapped(t *testing.T) {
	limiter, _ := newTestThrottler(t, testThrottleConfig())
	for last := 1; last <= 4; last++ {
		if !allowed(t, limiter, deriveLoginThrottleKey("ada@example.com", "192.0.2."+string(rune('0'+last)))) {
			t.Fatalf("source %d refused before the account cap", last)
		}
	}
	if allowed(t, limiter, deriveLoginThrottleKey("ada@example.com", "192.0.2.9")) {
		t.Fatal("a fifth source guessed the same login past the account cap")
	}
}

func TestSuccessClearsPairAndAccountButKeepsTheSourceCount(t *testing.T) {
	limiter, _ := newTestThrottler(t, testThrottleConfig())
	for _, login := range []string{"a", "b", "c", "d", "e", "f"} {
		key := deriveLoginThrottleKey(login+"@example.com", "192.0.2.1")
		if !allowed(t, limiter, key) {
			t.Fatalf("%s refused", login)
		}
		if err := limiter.Success(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	if allowed(t, limiter, deriveLoginThrottleKey("g@example.com", "192.0.2.1")) {
		t.Fatal("success cleared the source counter")
	}
	if !allowed(t, limiter, deriveLoginThrottleKey("g@example.com", "192.0.2.2")) {
		t.Fatal("another source was refused")
	}

	pair := deriveLoginThrottleKey("owner@example.com", "198.51.100.1")
	allowed(t, limiter, pair)
	allowed(t, limiter, pair)
	if err := limiter.Success(context.Background(), pair); err != nil {
		t.Fatal(err)
	}
	if !allowed(t, limiter, pair) {
		t.Fatal("success did not clear the pair counter")
	}
}

func TestThrottleSourceCountsIPv6ByPrefixAndMappedIPv4AsIPv4(t *testing.T) {
	for _, test := range []struct{ source, want string }{
		{"192.0.2.10", "192.0.2.10"},
		{" 192.0.2.10 ", "192.0.2.10"},
		{"::ffff:192.0.2.10", "192.0.2.10"},
		{"2001:db8:1:2:aaaa:bbbb:cccc:dddd", "2001:db8:1:2::/64"},
		{"2001:DB8:1:2::1", "2001:db8:1:2::/64"},
		{"fe80::1%eth0", "fe80::/64"},
		{"not an address", "not an address"},
		{"", ""},
	} {
		if got := throttleSource(test.source); got != test.want {
			t.Errorf("throttleSource(%q) = %q, want %q", test.source, got, test.want)
		}
	}
}

func TestIPv6SourcesAreOneSourcePerSlash64(t *testing.T) {
	limiter, _ := newTestThrottler(t, testThrottleConfig())
	for i, login := range []string{"a", "b", "c", "d", "e", "f"} {
		// Rotating through a /64 changes every address but not the source.
		source := "2001:db8:1:2::" + string(rune('1'+i))
		if !allowed(t, limiter, deriveLoginThrottleKey(login+"@example.com", source)) {
			t.Fatalf("%s refused", login)
		}
	}
	if allowed(t, limiter, deriveLoginThrottleKey("g@example.com", "2001:db8:1:2:ffff::1")) {
		t.Fatal("rotating addresses inside a /64 gave a fresh source budget")
	}
	if !allowed(t, limiter, deriveLoginThrottleKey("g@example.com", "2001:db8:1:3::1")) {
		t.Fatal("another /64 was refused")
	}
}

func TestThrottleKeysAreSeparatedByNamespaceAndBoundary(t *testing.T) {
	signIn := newThrottleKey("login", "ada@example.com", "192.0.2.1", true)
	if signIn == newThrottleKey("recovery", "ada@example.com", "192.0.2.1", true) {
		t.Fatal("sign-in and recovery share counters")
	}
	// ("ab", "c") and ("a", "bc") must not hash alike.
	if throttleDigest("x", "ab", "c") == throttleDigest("x", "a", "bc") {
		t.Fatal("throttle digests are ambiguous")
	}
	if key := newThrottleKey("login", "a", "", true); key.HasSource || key.HasPair {
		t.Fatalf("a key without a source has source or pair counters: %+v", key)
	}
}

func TestFullTableEvictsIdleCountersAndKeepsBlockingOnes(t *testing.T) {
	config := testThrottleConfig()
	config.MaxEntries = 9
	limiter, clock := newTestThrottler(t, config)

	attacker := deriveLoginThrottleKey("victim@example.com", "192.0.2.66")
	allowed(t, limiter, attacker)
	allowed(t, limiter, attacker) // the attacker's pair is now blocking (2 of 2)
	clock.Advance(time.Second)

	// Many sources fill the table with idle counters, then keep coming.
	for i := 0; i < 200; i++ {
		clock.Advance(time.Millisecond)
		key := deriveLoginThrottleKey(fmt.Sprintf("filler%d@example.com", i), fmt.Sprintf("198.51.%d.%d", i/250, i%250+1))
		if !allowed(t, limiter, key) {
			t.Fatalf("a full table refused attempt %d instead of evicting an idle counter", i)
		}
		if len(limiter.buckets) > config.MaxEntries {
			t.Fatalf("%d counters exceed MaxEntries=%d", len(limiter.buckets), config.MaxEntries)
		}
	}
	if allowed(t, limiter, attacker) {
		t.Fatal("filling the table flushed the attacker's blocking counter")
	}
	if got, want := len(limiter.buckets), limiter.order.Len(); got != want {
		t.Fatalf("map and order disagree: %d != %d", got, want)
	}
}

func TestFullTableOfBlockingCountersRefusesNewAttempts(t *testing.T) {
	config := testThrottleConfig()
	config.PairLimit = 1
	config.AccountLimit = 1
	config.SourceLimit = 1
	config.MaxEntries = 6
	limiter, _ := newTestThrottler(t, config)
	// Each attempt creates three counters, all blocking at once.
	for i := 0; i < 2; i++ {
		if !allowed(t, limiter, deriveLoginThrottleKey("a"+string(rune('a'+i))+"@example.com", "192.0.2."+string(rune('1'+i)))) {
			t.Fatalf("attempt %d refused", i)
		}
	}
	decision, err := limiter.Attempt(context.Background(), deriveLoginThrottleKey("new@example.com", "192.0.2.9"))
	if err != nil || decision.Allowed || decision.RetryAfter != config.Window {
		t.Fatalf("decision=%+v err=%v, want a refusal for the whole window", decision, err)
	}
	if len(limiter.buckets) != 6 {
		t.Fatalf("a refused attempt changed the table: %d counters", len(limiter.buckets))
	}
}

func TestExpiredCountersAreRemovedAndBlockingCountIsExact(t *testing.T) {
	limiter, clock := newTestThrottler(t, testThrottleConfig())
	key := deriveLoginThrottleKey("a@example.com", "192.0.2.1")
	allowed(t, limiter, key)
	allowed(t, limiter, key)
	if limiter.blocking != 1 {
		t.Fatalf("blocking=%d, want the pair only", limiter.blocking)
	}
	clock.Advance(time.Minute)
	if !allowed(t, limiter, key) {
		t.Fatal("a counter whose window ended still blocks")
	}
	if limiter.blocking != 0 || len(limiter.buckets) != 3 || limiter.order.Len() != 3 {
		t.Fatalf("blocking=%d counters=%d order=%d", limiter.blocking, len(limiter.buckets), limiter.order.Len())
	}
	if err := limiter.Success(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if len(limiter.buckets) != 1 || limiter.blocking != 0 {
		t.Fatalf("after success counters=%d blocking=%d, want the source counter only", len(limiter.buckets), limiter.blocking)
	}
}

func TestThrottleConfigValidation(t *testing.T) {
	valid := testThrottleConfig()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*LoginThrottleConfig){
		"zero window":              func(c *LoginThrottleConfig) { c.Window = 0 },
		"zero account limit":       func(c *LoginThrottleConfig) { c.AccountLimit = 0 },
		"zero source limit":        func(c *LoginThrottleConfig) { c.SourceLimit = 0 },
		"negative pair limit":      func(c *LoginThrottleConfig) { c.PairLimit = -1 },
		"pair above account limit": func(c *LoginThrottleConfig) { c.PairLimit = c.AccountLimit + 1 },
		"fewer than three entries": func(c *LoginThrottleConfig) { c.MaxEntries = 2 },
	} {
		config := valid
		mutate(&config)
		if err := config.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	legacy := LoginThrottleConfig{Window: time.Minute, AccountLimit: 1, SourceLimit: 10, MaxEntries: 10}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("a config without a pair limit must stay valid: %v", err)
	}
	if defaults := DefaultLoginThrottleConfig(); defaults.PairLimit != 5 || defaults.AccountLimit != 50 || defaults.SourceLimit != 300 || defaults.Window != 15*time.Minute {
		t.Fatalf("defaults=%+v", defaults)
	}
}
