package mail_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/mail"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

func TestInstrumentedSender(t *testing.T) {
	tel, recorder := telemetrytest.New(t)
	calls := 0
	base := mail.SenderFunc(func(context.Context, mail.Message) error {
		calls++
		if calls == 2 {
			return fmt.Errorf("relay said 451 for bob@example.com: %w", mail.ErrUnavailable)
		}
		return nil
	})
	sender, err := mail.Instrument(base, tel)
	if err != nil {
		t.Fatal(err)
	}
	ctx, parent := tel.Tracer("test").Start(context.Background(), "signup", telemetry.SpanKindServer)
	if err := sender.Send(ctx, mail.Message{Subject: "Welcome bob"}); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(ctx, mail.Message{}); err == nil {
		t.Fatal("errors must pass through")
	}
	if err := sender.Send(context.Background(), mail.Message{}); err != nil {
		t.Fatal(err)
	}
	parent.End()

	series := telemetrytest.Series(tel, mail.MetricSendDuration)
	if series["forge.mail.outcome=sent"] != 2 || series["forge.mail.outcome=mail_unavailable"] != 1 {
		t.Fatalf("series %v", series)
	}
	spans := recorder.Spans(t, tel)
	if len(spans) != 3 {
		t.Fatalf("spans %d: only traced sends get spans", len(spans))
	}
	for _, span := range spans {
		for _, attr := range span.Attributes {
			if strings.Contains(attr.Value.Emit(), "bob") {
				t.Fatalf("recipient or subject recorded: %+v", attr)
			}
		}
	}
	unchanged, err := mail.Instrument(base, nil)
	if err != nil || unchanged == nil {
		t.Fatal("a nil Telemetry returns the sender unchanged")
	}
}
