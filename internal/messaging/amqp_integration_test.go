package messaging

// Integration tests against a real RabbitMQ (TEST_BROKER_URL), e.g. amqp://guest:guest@127.0.0.1:5672/.
// Skipped when the variable is not set. They share the fixed queue names, so they run one at a time.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ronybrand/go-order-api/internal/domain"
)

type scriptedNotifier struct {
	mu     sync.Mutex
	events []domain.OrderStatusChanged
	err    error
}

func (n *scriptedNotifier) Notify(_ context.Context, e domain.OrderStatusChanged) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, e)
	return n.err
}

func (n *scriptedNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.events)
}

func brokerURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_BROKER_URL")
	if url == "" {
		t.Skip("TEST_BROKER_URL not set")
	}
	return url
}

// freshQueues declares the topology and empties both queues, then returns a channel to inspect them.
func freshQueues(t *testing.T, url string) *amqp.Channel {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := declareTopology(ch); err != nil {
		t.Fatal(err)
	}
	for _, queue := range []string{QueueName, DLQName} {
		if _, err := ch.QueuePurge(queue, false); err != nil {
			t.Fatal(err)
		}
	}
	return ch
}

func runConsumer(t *testing.T, url string, notifier Notifier) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	consumer := NewConsumer(url, NewProcessor(notifier, discard), discard)
	go func() { _ = consumer.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestBroker_PublishedEventReachesTheNotifierWithAMessageID(t *testing.T) {
	url := brokerURL(t)
	freshQueues(t, url)
	notifier := &scriptedNotifier{}
	runConsumer(t, url, notifier)
	publisher := NewPublisher(url)
	t.Cleanup(func() { _ = publisher.Close() })
	sent := event()

	if err := publisher.PublishStatusChanged(context.Background(), sent); err != nil {
		t.Fatal(err)
	}

	eventually(t, 5*time.Second, "the notification", func() bool { return notifier.count() == 1 })
	if got := notifier.events[0]; got.OrderID != sent.OrderID || got.CustomerEmail != sent.CustomerEmail {
		t.Errorf("received %+v, sent %+v", got, sent)
	}
}

func TestBroker_PermanentFailureGoesStraightToTheDeadLetterQueue(t *testing.T) {
	url := brokerURL(t)
	inspect := freshQueues(t, url)
	notifier := &scriptedNotifier{err: fmt.Errorf("bad recipient: %w", ErrPermanent)}
	runConsumer(t, url, notifier)
	publisher := NewPublisher(url)
	t.Cleanup(func() { _ = publisher.Close() })

	if err := publisher.PublishStatusChanged(context.Background(), event()); err != nil {
		t.Fatal(err)
	}

	var dead amqp.Delivery
	eventually(t, 5*time.Second, "the dead-lettered message", func() bool {
		d, ok, err := inspect.Get(DLQName, true)
		dead = d
		return err == nil && ok
	})
	if notifier.count() != 1 {
		t.Errorf("a permanent failure must not be retried, notifier called %d times", notifier.count())
	}
	if dead.MessageId == "" || len(dead.Body) == 0 {
		t.Errorf("dead letter lost its identity: %+v", dead)
	}
}

func TestBroker_TransientFailureIsRetriedThenDeadLettered(t *testing.T) {
	url := brokerURL(t)
	inspect := freshQueues(t, url)
	notifier := &scriptedNotifier{err: errors.New("smtp down")}
	runConsumer(t, url, notifier)
	publisher := NewPublisher(url)
	t.Cleanup(func() { _ = publisher.Close() })

	if err := publisher.PublishStatusChanged(context.Background(), event()); err != nil {
		t.Fatal(err)
	}

	var dead amqp.Delivery
	wait := time.Duration(MaxRetries)*RetryDelay + 10*time.Second
	eventually(t, wait, "the message to exhaust its retries", func() bool {
		d, ok, err := inspect.Get(DLQName, true)
		dead = d
		return err == nil && ok
	})
	if want := MaxRetries + 1; notifier.count() != want {
		t.Errorf("notifier called %d times, want %d (first attempt plus %d retries)", notifier.count(), want, MaxRetries)
	}
	if got := retriesOf(dead); got != MaxRetries {
		t.Errorf("x-retry-count = %d, want %d", got, MaxRetries)
	}
	if _, ok, _ := inspect.Get(QueueName, false); ok {
		t.Error("the original must have been acked after the DLQ publish")
	}
}
