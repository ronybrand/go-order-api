// Package messaging carries order status changes from the API to the notification worker: the AMQP
// producer and consumer, the retry/dead-letter policy, and the e-mail rendering and delivery.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ronybrand/go-order-api/internal/domain"
)

// Queue topology and retry policy (DOMAIN.md section 5).
const (
	QueueName  = "order.status.changed"
	DLQName    = QueueName + ".dlq"
	MaxRetries = 3
	RetryDelay = 2 * time.Second

	retryCountHeader = "x-retry-count"
	seenTTL          = time.Hour
	seenCapacity     = 10_000
)

// ErrPermanent marks a failure that retrying cannot fix (an invalid recipient, say): the message goes
// straight to the dead-letter queue.
var ErrPermanent = errors.New("permanent failure")

// Message is a delivery as the processor sees it, independent of the AMQP library.
type Message struct {
	ID      string // AMQP message_id, used to drop redeliveries
	Body    []byte
	Retries int // value of the x-retry-count header
}

// Action is what the consumer must do with the delivery.
type Action int

// The possible outcomes of processing a message.
const (
	Ack        Action = iota // done, or a duplicate: acknowledge
	Retry                    // transient failure: republish with the retry count incremented, after RetryDelay
	DeadLetter               // unrecoverable: publish to the DLQ
)

func (a Action) String() string {
	return [...]string{"ack", "retry", "dead-letter"}[a]
}

// Notifier delivers the notification of one event.
type Notifier interface {
	Notify(ctx context.Context, event domain.OrderStatusChanged) error
}

// Processor decides what happens to each message. It owns no connection, so the retry, dedupe and
// dead-letter rules are tested without a broker.
type Processor struct {
	notifier Notifier
	seen     *seenSet
	logger   *slog.Logger
}

// NewProcessor builds a processor.
func NewProcessor(notifier Notifier, logger *slog.Logger) *Processor {
	return &Processor{notifier: notifier, seen: newSeenSet(seenTTL, seenCapacity, time.Now), logger: logger}
}

// Process handles one message. Delivery is at-least-once, so a message already completed (same message_id)
// is acknowledged without notifying again; only a successful notification marks the id as done.
func (p *Processor) Process(ctx context.Context, msg Message) Action {
	var event domain.OrderStatusChanged
	if err := json.Unmarshal(msg.Body, &event); err != nil || event.OrderID == uuid.Nil {
		p.logger.ErrorContext(ctx, "discarding malformed message", "message_id", msg.ID)
		return DeadLetter
	}
	if msg.ID != "" && p.seen.has(msg.ID) {
		p.logger.InfoContext(ctx, "duplicate message ignored", "message_id", msg.ID)
		return Ack
	}
	err := p.notifier.Notify(ctx, event)
	switch {
	case err == nil:
		if msg.ID != "" {
			p.seen.add(msg.ID)
		}
		p.logger.InfoContext(ctx, "notification sent", "event", event, "message_id", msg.ID)
		return Ack
	case errors.Is(err, ErrPermanent):
		p.logger.ErrorContext(ctx, "notification failed permanently", "event", event, "error", err)
		return DeadLetter
	case msg.Retries >= MaxRetries:
		p.logger.ErrorContext(ctx, "notification failed, retries exhausted", "event", event, "error", err, "retries", msg.Retries)
		return DeadLetter
	default:
		p.logger.WarnContext(ctx, "notification failed, will retry", "event", event, "error", err, "retries", msg.Retries)
		return Retry
	}
}

// seenSet remembers recently completed message ids: bounded in size and in time, so it cannot leak memory.
type seenSet struct {
	ttl      time.Duration
	capacity int
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]time.Time
}

func newSeenSet(ttl time.Duration, capacity int, now func() time.Time) *seenSet {
	return &seenSet{ttl: ttl, capacity: capacity, now: now, entries: make(map[string]time.Time)}
}

func (s *seenSet) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, found := s.entries[id]
	if found && s.now().Sub(at) > s.ttl {
		delete(s.entries, id)
		return false
	}
	return found
}

func (s *seenSet) add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.entries) >= s.capacity {
		s.evict(now)
	}
	s.entries[id] = now
}

// evict drops the expired entries and, if that was not enough, the oldest one.
func (s *seenSet) evict(now time.Time) {
	var oldestID string
	var oldest time.Time
	for id, at := range s.entries {
		if now.Sub(at) > s.ttl {
			delete(s.entries, id)
			continue
		}
		if oldestID == "" || at.Before(oldest) {
			oldestID, oldest = id, at
		}
	}
	if len(s.entries) >= s.capacity && oldestID != "" {
		delete(s.entries, oldestID)
	}
}

func (s *seenSet) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
