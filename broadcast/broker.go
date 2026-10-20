package broadcast

import (
	"context"
	"errors"
	"fmt"

	"github.com/rocketinsidecorp/forge-go/turbo"
)

// Errors reported by brokers and subscriptions.
var (
	// ErrClosed means the broker was shut down.
	ErrClosed = errors.New("broadcast: broker closed")
	// ErrTooManySubscribers means a subscriber limit was reached.
	ErrTooManySubscribers = errors.New("broadcast: too many subscribers")
	// ErrSlowConsumer is the Err of a subscription dropped because its buffer
	// was full: the consumer was not reading fast enough.
	ErrSlowConsumer = errors.New("broadcast: subscriber too slow")
	// ErrMessageTooLarge means a message exceeds the broker's limit.
	ErrMessageTooLarge = errors.New("broadcast: message too large")
)

// Message is one payload for the subscribers of a channel: the text of one
// or more <turbo-stream> elements.
type Message struct {
	Data string
}

// Subscription is a live subscription to one channel.
type Subscription interface {
	// Messages delivers messages in publish order. The channel is closed when
	// the subscription ends; Err then says why.
	Messages() <-chan Message
	// Err is nil after Close, ErrSlowConsumer when the subscriber was dropped
	// for not keeping up, and ErrClosed when the broker shut down. It is only
	// meaningful once Messages is closed.
	Err() error
	// Close ends the subscription. It is idempotent.
	Close()
}

// Broker fans messages out to subscribers. Implementations must be safe for
// concurrent use, bound their memory, and never block Publish on a slow
// subscriber.
type Broker interface {
	// Publish delivers message to the current subscribers of channel. It
	// never waits for subscribers; with no subscribers it is a no-op.
	Publish(ctx context.Context, channel Channel, message Message) error
	// Subscribe starts receiving messages published after it returns, until
	// ctx is cancelled or the subscription is closed or dropped.
	Subscribe(ctx context.Context, channel Channel) (Subscription, error)
}

// Publish renders streams and publishes them to channel. It is the usual way
// to broadcast: build streams with package turbo (content from
// views.Renderer.Fragment) and publish them after the change they describe
// has been committed. The same HTML goes to every subscriber, so it must not
// contain anything specific to the publishing request (CSRF tokens, the
// current user).
func Publish(ctx context.Context, broker Broker, channel Channel, streams ...turbo.Stream) error {
	if broker == nil {
		return errors.New("broadcast: a broker is required")
	}
	if err := channel.valid(); err != nil {
		return err
	}
	if len(streams) == 0 {
		return errors.New("broadcast: at least one stream is required")
	}
	if err := broker.Publish(ctx, channel, Message{Data: string(turbo.Join(streams...))}); err != nil {
		return fmt.Errorf("broadcast: publish to %s: %w", channel.Key(), err)
	}
	return nil
}
