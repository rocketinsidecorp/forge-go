package mail

import (
	"context"
	"time"

	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// MetricSendDuration is the histogram of mail deliveries.
const MetricSendDuration = "forge.mail.send.duration"

// Instrument returns a Sender that measures every Send in
// forge.mail.send.duration (label: outcome, "sent" or the error's fault code
// such as mail_unavailable) and, inside a traced operation, records a client
// span. Recipients, subjects, bodies, headers and error messages are never
// recorded. With a nil Telemetry it returns sender unchanged.
func Instrument(sender Sender, t *telemetry.Telemetry) (Sender, error) {
	if t == nil || sender == nil {
		return sender, nil
	}
	duration, err := t.Metrics().Histogram(MetricSendDuration, "Duration of mail deliveries.", "s", telemetry.DurationBuckets, "forge.mail.outcome")
	if err != nil {
		return nil, err
	}
	return &instrumentedSender{next: sender, tracer: t.Tracer(telemetry.ScopeName + "/mail"), duration: duration}, nil
}

type instrumentedSender struct {
	next     Sender
	tracer   *telemetry.Tracer
	duration *telemetry.Histogram
}

func (s *instrumentedSender) Send(ctx context.Context, msg Message) error {
	started := time.Now()
	var span *telemetry.Span
	if telemetry.SpanContextFromContext(ctx).IsValid() {
		ctx, span = s.tracer.Start(ctx, "mail send", telemetry.SpanKindClient)
	}
	err := s.next.Send(ctx, msg)
	outcome := "sent"
	if err != nil {
		outcome = telemetry.ErrorType(err)
		span.RecordError(err)
	}
	span.End()
	s.duration.Record(time.Since(started).Seconds(), outcome)
	return err
}
