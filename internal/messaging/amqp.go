package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/domain"
)

const (
	publishTimeout = 5 * time.Second
	reconnectDelay = 5 * time.Second
)

// declareTopology creates the queues idempotently: the main queue dead-letters to the DLQ through the
// default exchange, and both survive a broker restart.
func declareTopology(ch *amqp.Channel) error {
	if _, err := ch.QueueDeclare(DLQName, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare %s: %w", DLQName, err)
	}
	_, err := ch.QueueDeclare(QueueName, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": DLQName,
	})
	if err != nil {
		return fmt.Errorf("declare %s: %w", QueueName, err)
	}
	return nil
}

// Publisher is the app.EventPublisher backed by RabbitMQ. It dials lazily and redials after the connection
// drops, so an unavailable broker at startup does not stop the API (publishing is best effort anyway).
type Publisher struct {
	url string

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

var _ app.EventPublisher = (*Publisher)(nil)

// NewPublisher builds a publisher for the broker URL (amqps:// in production).
func NewPublisher(url string) *Publisher { return &Publisher{url: url} }

func (p *Publisher) channel() (*amqp.Channel, error) {
	if p.ch != nil && !p.ch.IsClosed() {
		return p.ch, nil
	}
	if p.conn != nil && !p.conn.IsClosed() {
		_ = p.conn.Close()
	}
	conn, err := amqp.DialConfig(p.url, amqp.Config{Dial: amqp.DefaultDial(publishTimeout)})
	if err != nil {
		return nil, fmt.Errorf("dial broker: %w", err)
	}
	ch, err := conn.Channel()
	if err == nil {
		err = declareTopology(ch)
	}
	if err == nil {
		err = ch.Confirm(false) // publisher confirms: a publish is only successful once the broker has it
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	p.conn, p.ch = conn, ch
	return ch, nil
}

// PublishStatusChanged queues the event as a persistent message with a unique message_id.
func (p *Publisher) PublishStatusChanged(ctx context.Context, event domain.OrderStatusChanged) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	p.mu.Lock()
	defer p.mu.Unlock()
	ch, err := p.channel()
	if err != nil {
		return err
	}
	confirmation, err := ch.PublishWithDeferredConfirmWithContext(ctx, "", QueueName, false, false, amqp.Publishing{
		ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: uuid.NewString(),
		Timestamp: time.Now().UTC(), Body: body,
	})
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	acked, err := confirmation.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("wait for broker confirmation: %w", err)
	}
	if !acked {
		return errors.New("broker rejected the message")
	}
	return nil
}

// Close releases the connection.
func (p *Publisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil || p.conn.IsClosed() {
		return nil
	}
	return p.conn.Close()
}

// Consumer reads the queue and applies the Processor's decision to each delivery.
type Consumer struct {
	url       string
	processor *Processor
	logger    *slog.Logger
}

// NewConsumer builds the consumer.
func NewConsumer(url string, processor *Processor, logger *slog.Logger) *Consumer {
	return &Consumer{url: url, processor: processor, logger: logger}
}

// Run consumes until ctx is canceled, reconnecting after a broker failure.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		err := c.consume(ctx)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // a canceled context is the normal shutdown, not a failure
		}
		c.logger.ErrorContext(ctx, "consumer stopped, reconnecting", "error", err, "in", reconnectDelay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(reconnectDelay):
		}
	}
}

func (c *Consumer) consume(ctx context.Context) error {
	conn, err := amqp.DialConfig(c.url, amqp.Config{Dial: amqp.DefaultDial(publishTimeout)})
	if err != nil {
		return fmt.Errorf("dial broker: %w", err)
	}
	defer conn.Close() //nolint:errcheck // closing on the way out

	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := declareTopology(ch); err != nil {
		return err
	}
	// One unacknowledged message at a time: the fixed retry delay below then really throttles the queue.
	if err := ch.Qos(1, 0, false); err != nil {
		return err
	}
	deliveries, err := ch.ConsumeWithContext(ctx, QueueName, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	c.logger.InfoContext(ctx, "consuming", "queue", QueueName)
	for {
		select {
		case <-ctx.Done():
			return nil
		case d, open := <-deliveries:
			if !open {
				return errors.New("delivery channel closed")
			}
			c.handle(ctx, ch, d)
		}
	}
}

func retriesOf(d amqp.Delivery) int {
	switch v := d.Headers[retryCountHeader].(type) {
	case int32:
		return int(v)
	case int64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func (c *Consumer) handle(ctx context.Context, ch *amqp.Channel, d amqp.Delivery) {
	retries := retriesOf(d)
	action := c.processor.Process(ctx, Message{ID: d.MessageId, Body: d.Body, Retries: retries})
	var err error
	switch action {
	case Ack:
	case Retry:
		select {
		case <-ctx.Done():
			_ = d.Nack(false, true) // shutting down: leave the message for the next start
			return
		case <-time.After(RetryDelay):
		}
		err = republish(ctx, ch, d, QueueName, retries+1)
	case DeadLetter:
		err = republish(ctx, ch, d, DLQName, retries)
	}
	if err != nil {
		c.logger.ErrorContext(ctx, "could not move message, requeueing", "action", action.String(), "error", err)
		_ = d.Nack(false, true)
		return
	}
	if err := d.Ack(false); err != nil {
		c.logger.ErrorContext(ctx, "ack failed", "error", err)
	}
}

// republish copies the delivery (same body and message_id, so dedupe still works) to another queue.
func republish(ctx context.Context, ch *amqp.Channel, d amqp.Delivery, queue string, retries int) error {
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}
	headers[retryCountHeader] = int32(retries) //nolint:gosec // retries is bounded by MaxRetries
	return ch.PublishWithContext(ctx, "", queue, false, false, amqp.Publishing{
		ContentType: d.ContentType, DeliveryMode: amqp.Persistent, MessageId: d.MessageId,
		Timestamp: d.Timestamp, Headers: headers, Body: d.Body,
	})
}
