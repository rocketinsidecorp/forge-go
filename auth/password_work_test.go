package auth

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// holdSlot takes one slot of limiter until the returned release is called.
func holdSlot(t *testing.T, limiter *PasswordWorkLimiter) (release func()) {
	t.Helper()
	held, done := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- limiter.Do(context.Background(), func() {
			close(held)
			<-done
		})
	}()
	select {
	case <-held:
	case err := <-finished:
		t.Fatalf("could not take a slot: %v", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			if err := <-finished; err != nil {
				t.Errorf("held slot: %v", err)
			}
		})
	}
}

func TestPasswordWorkLimiterNeverExceedsItsBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter, err := NewPasswordWorkLimiter(3, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var running, peak atomic.Int64
		var wg sync.WaitGroup
		for range 24 {
			wg.Go(func() {
				err := limiter.Do(context.Background(), func() {
					now := running.Add(1)
					for {
						seen := peak.Load()
						if now <= seen || peak.CompareAndSwap(seen, now) {
							break
						}
					}
					// fake-clock: holds the slot for 20 ms of bubble time so the 24 attempts overlap
					time.Sleep(20 * time.Millisecond)
					running.Add(-1)
				})
				if err != nil {
					t.Errorf("attempt: %v", err)
				}
			})
		}
		wg.Wait()
		if peak.Load() != 3 || running.Load() != 0 {
			t.Fatalf("peak=%d running=%d, want the bound 3 reached and never passed", peak.Load(), running.Load())
		}
	})
}

func TestPasswordWorkLimiterFailsAfterItsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter, err := NewPasswordWorkLimiter(1, DefaultPasswordWorkWait)
		if err != nil {
			t.Fatal(err)
		}
		release := holdSlot(t, limiter)
		start := time.Now()
		ran := false
		if err := limiter.Do(context.Background(), func() { ran = true }); !errors.Is(err, ErrPasswordHashingUnavailable) || ran {
			t.Fatalf("Do on a full limiter: err=%v ran=%v", err, ran)
		}
		if waited := time.Since(start); waited != DefaultPasswordWorkWait {
			t.Fatalf("waited %v, want %v", waited, DefaultPasswordWorkWait)
		}
		// A slot freed while an attempt waits is taken by it.
		go func() {
			// fake-clock: frees the slot one bubble second into the next attempt's wait
			time.Sleep(time.Second)
			release()
		}()
		if err := limiter.Do(context.Background(), func() { ran = true }); err != nil || !ran {
			t.Fatalf("Do after release: err=%v ran=%v", err, ran)
		}
	})
}

func TestPasswordWorkLimiterZeroWaitCancellationAndPanics(t *testing.T) {
	t.Parallel()
	limiter, err := NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	release := holdSlot(t, limiter)
	if err := limiter.Do(context.Background(), func() { t.Error("work ran without a slot") }); !errors.Is(err, ErrPasswordHashingUnavailable) {
		t.Fatalf("zero wait: %v", err)
	}
	release()

	waiting, err := NewPasswordWorkLimiter(1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	release = holdSlot(t, waiting)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waiting.Do(ctx, func() { t.Error("work ran without a slot") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
	release()

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the work's panic was swallowed")
			}
		}()
		_ = limiter.Do(context.Background(), func() { panic("hash failed") })
	}()
	if err := limiter.Do(context.Background(), func() {}); err != nil {
		t.Fatalf("a panicking hash kept its slot: %v", err)
	}

	if _, err := NewPasswordWorkLimiter(0, time.Second); err == nil {
		t.Error("zero slots accepted")
	}
	if _, err := NewPasswordWorkLimiter(1, -time.Second); err == nil {
		t.Error("negative wait accepted")
	}
}

func TestDefaultPasswordWorkLimiterFollowsTheCPUsWithinLimits(t *testing.T) {
	t.Parallel()
	limiter := DefaultPasswordWorkLimiter()
	if limiter != DefaultPasswordWorkLimiter() || DefaultPasswordHasher().work != limiter {
		t.Fatal("the default limiter is not shared by the process")
	}
	want := min(runtime.GOMAXPROCS(0), MaxDefaultPasswordWork)
	if limiter.MaxConcurrent() != want || limiter.maxWait != DefaultPasswordWorkWait {
		t.Fatalf("slots=%d wait=%v, want %d and %v", limiter.MaxConcurrent(), limiter.maxWait, want, DefaultPasswordWorkWait)
	}
	if MaxDefaultPasswordWork != 16 || DefaultPasswordWorkWait != 2*time.Second {
		t.Fatal("the defaults drifted from Forge-Rust's MAX_DEFAULT_PASSWORD_WORK and DEFAULT_PASSWORD_WORK_WAIT")
	}
	if got := ErrPasswordHashingUnavailable.Status(); got != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", got)
	}
	if fault.From(ErrPasswordHashingUnavailable).Code != "authentication_unavailable" {
		t.Fatal("code drifted from Forge-Rust's authentication_unavailable")
	}
}

// Hashing, verifying and the dummy verification of an unknown account all
// take a slot.
func TestPasswordHasherRunsEveryHashThroughItsLimiter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	limiter, err := NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, algorithm := range []PasswordAlgorithm{PasswordAlgorithmArgon2id, PasswordAlgorithmPBKDF2SHA256} {
		hasher := cheapPasswordHasher(t, WithPasswordWorkLimiter(limiter), WithPasswordAlgorithm(algorithm))
		stored, err := hasher.Hash(ctx, "secret")
		if err != nil {
			t.Fatal(err)
		}
		release := holdSlot(t, limiter)
		if _, err := hasher.Hash(ctx, "secret"); !errors.Is(err, ErrPasswordHashingUnavailable) {
			t.Errorf("%s Hash: %v", algorithm, err)
		}
		if _, _, err := hasher.Verify(ctx, stored, "secret"); !errors.Is(err, ErrPasswordHashingUnavailable) {
			t.Errorf("%s Verify: %v", algorithm, err)
		}
		if err := hasher.verifyDummy(ctx, "secret"); !errors.Is(err, ErrPasswordHashingUnavailable) {
			t.Errorf("%s dummy: %v", algorithm, err)
		}
		release()
	}
}
