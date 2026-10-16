package mailtest_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	forgemail "github.com/rocketinsidecorp/forge-go/mail"
	"github.com/rocketinsidecorp/forge-go/mail/mailtest"
)

func message(subject string) forgemail.Message {
	return forgemail.Message{
		From:    forgemail.Address{Email: "app@example.com"},
		To:      []forgemail.Address{{Email: "alice@example.org"}},
		Bcc:     []forgemail.Address{{Email: "audit@example.net"}},
		Subject: subject,
		Text:    "hello",
	}
}

func TestRecorderRecordsDeliveries(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	if _, ok := recorder.Last(); ok || recorder.Len() != 0 {
		t.Fatal("new recorder is not empty")
	}
	if err := recorder.Send(context.Background(), message("first")); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Send(context.Background(), message("second")); err != nil {
		t.Fatal(err)
	}
	deliveries := recorder.Deliveries()
	if len(deliveries) != 2 || deliveries[0].Message.Subject != "first" || deliveries[1].Message.Subject != "second" {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	last, _ := recorder.Last()
	if last.Message.Subject != "second" || last.Sender != "app@example.com" || len(last.Recipients) != 2 || last.MessageID == "" || last.At.IsZero() {
		t.Fatalf("last = %+v", last)
	}
	parsed, err := last.Parse()
	if err != nil || parsed.Header.Get("Subject") != "second" || parsed.Header.Get("Bcc") != "" {
		t.Fatalf("Parse = %v, %v", parsed, err)
	}
	recorder.Reset()
	if recorder.Len() != 0 {
		t.Fatal("Reset did not clear deliveries")
	}
}

func TestRecorderRejectsWhatProductionRejects(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	bad := message("x\r\nBcc: evil@example.net")
	if err := recorder.Send(context.Background(), bad); !errors.Is(err, forgemail.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	noRecipient := message("ok")
	noRecipient.To, noRecipient.Bcc = nil, nil
	if err := recorder.Send(context.Background(), noRecipient); !errors.Is(err, forgemail.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if recorder.Len() != 0 {
		t.Fatal("invalid messages were recorded")
	}
}

func TestRecorderCopiesMessagesAndSnapshots(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	msg := message("subject")
	if err := recorder.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	msg.To[0].Email = "mutated@example.org"
	snapshot := recorder.Deliveries()
	snapshot[0].Message.To[0].Email = "mutated2@example.org"
	snapshot[0].Raw[0] = 'X'
	again, _ := recorder.Last()
	if again.Message.To[0].Email != "alice@example.org" || again.Raw[0] == 'X' {
		t.Fatalf("recorder state was mutated through aliases: %+v", again.Message.To)
	}
}

func TestRecorderFailWithAndContext(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	boom := errors.New("smtp down")
	recorder.FailWith(boom)
	if err := recorder.Send(context.Background(), message("x")); !errors.Is(err, boom) || recorder.Len() != 0 {
		t.Fatalf("err = %v, len = %d", err, recorder.Len())
	}
	recorder.FailWith(nil)
	if err := recorder.Send(context.Background(), message("x")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := recorder.Send(ctx, message("y")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestRecorderIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	recorder := mailtest.NewRecorder()
	const workers, each = 16, 25
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := recorder.Send(context.Background(), message(fmt.Sprintf("w%d-%d", w, i))); err != nil {
					t.Error(err)
					return
				}
				_ = recorder.Len()
				_ = recorder.Deliveries()
				_, _ = recorder.Last()
			}
		}()
	}
	wg.Wait()
	if recorder.Len() != workers*each {
		t.Fatalf("len = %d, want %d", recorder.Len(), workers*each)
	}
	ids := make(map[string]bool)
	for _, d := range recorder.Deliveries() {
		if ids[d.MessageID] {
			t.Fatalf("duplicate Message-ID %s", d.MessageID)
		}
		ids[d.MessageID] = true
	}
}
