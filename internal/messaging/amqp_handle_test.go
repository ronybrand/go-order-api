package messaging

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ronybrand/go-order-api/internal/domain"
)

// fakeAcker records how a delivery was settled.
type fakeAcker struct {
	acked, nacked, requeued int
}

func (a *fakeAcker) Ack(uint64, bool) error { a.acked++; return nil }
func (a *fakeAcker) Nack(_ uint64, _ bool, requeue bool) error {
	a.nacked++
	if requeue {
		a.requeued++
	}
	return nil
}
func (a *fakeAcker) Reject(uint64, bool) error { return nil }

// fakeMover records the confirmed publishes the consumer asks for.
type fakeMover struct {
	queues []string
	err    error
}

func (m *fakeMover) publish(_ context.Context, queue string, _ amqp.Publishing) error {
	m.queues = append(m.queues, queue)
	return m.err
}

type stubNotifier struct{ err error }

func (n stubNotifier) Notify(context.Context, domain.OrderStatusChanged) error { return n.err }

func newTestConsumer(notifyErr error) *Consumer {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewConsumer("amqp://unused", NewProcessor(stubNotifier{err: notifyErr}, logger), logger)
}

const validBody = `{"order_id":"00000000-0000-0000-0000-0000000000bb"}`

func TestHandle_AcksAMovedMessageOnlyAfterTheBrokerConfirmedIt(t *testing.T) {
	tests := map[string]struct {
		notifyErr  error
		moverErr   error
		wantQueues []string
		wantAck    int
		wantNack   int
	}{
		"sent: nothing to move, ack":                 {wantAck: 1},
		"permanent failure goes to the DLQ, ack":     {notifyErr: ErrPermanent, wantQueues: []string{DLQName}, wantAck: 1},
		"unconfirmed DLQ publish is requeued":        {notifyErr: ErrPermanent, moverErr: errors.New("broker rejected the message"), wantQueues: []string{DLQName}, wantNack: 1},
		"unconfirmed retry publish is requeued, too": {notifyErr: errors.New("smtp down"), moverErr: errors.New("wait for broker confirmation"), wantQueues: []string{QueueName}, wantNack: 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			acker := &fakeAcker{}
			mover := &fakeMover{err: tt.moverErr}
			delivery := amqp.Delivery{Acknowledger: acker, MessageId: "m1", Body: []byte(validBody)}

			newTestConsumer(tt.notifyErr).handle(context.Background(), mover, delivery)

			if acker.acked != tt.wantAck || acker.nacked != tt.wantNack || acker.requeued != tt.wantNack {
				t.Errorf("ack=%d nack=%d requeued=%d, want ack=%d nack=%d requeued=%d",
					acker.acked, acker.nacked, acker.requeued, tt.wantAck, tt.wantNack, tt.wantNack)
			}
			if len(mover.queues) != len(tt.wantQueues) || (len(tt.wantQueues) == 1 && mover.queues[0] != tt.wantQueues[0]) {
				t.Errorf("published to %v, want %v", mover.queues, tt.wantQueues)
			}
		})
	}
}
