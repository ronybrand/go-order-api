package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ronybrand/go-order-api/internal/domain"
)

var discard = slog.New(slog.DiscardHandler)

type fakeNotifier struct {
	calls []domain.OrderStatusChanged
	err   error
}

func (f *fakeNotifier) Notify(_ context.Context, e domain.OrderStatusChanged) error {
	f.calls = append(f.calls, e)
	return f.err
}

func event() domain.OrderStatusChanged {
	return domain.OrderStatusChanged{
		OrderID: uuid.New(), CustomerEmail: "ana@example.com", CustomerName: "Ana",
		OldStatus: domain.StatusOpen, NewStatus: domain.StatusConfirmed,
		TotalAmount: decimal.RequireFromString("10.30"), ChangedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func message(t *testing.T, id string, retries int) Message {
	t.Helper()
	body, err := json.Marshal(event())
	if err != nil {
		t.Fatal(err)
	}
	return Message{ID: id, Body: body, Retries: retries}
}

func TestEventWireFormat_RoundTrips(t *testing.T) {
	in := event()

	body, _ := json.Marshal(in)
	var out domain.OrderStatusChanged
	err := json.Unmarshal(body, &out)

	if err != nil || out.OrderID != in.OrderID || !out.TotalAmount.Equal(in.TotalAmount) || out.NewStatus != in.NewStatus || !out.ChangedAt.Equal(in.ChangedAt) {
		t.Errorf("out = %+v err = %v", out, err)
	}
	if !strings.Contains(string(body), `"order_id"`) || !strings.Contains(string(body), `"customer_email"`) {
		t.Errorf("wire format must be snake_case: %s", body)
	}
}

func TestProcess_AcksAfterNotifying(t *testing.T) {
	n := &fakeNotifier{}
	p := NewProcessor(n, discard)

	action := p.Process(context.Background(), message(t, "m1", 0))

	if action != Ack || len(n.calls) != 1 || n.calls[0].CustomerEmail != "ana@example.com" {
		t.Errorf("action=%v calls=%d", action, len(n.calls))
	}
}

func TestProcess_DeduplicatesByMessageID(t *testing.T) {
	n := &fakeNotifier{}
	p := NewProcessor(n, discard)

	first := p.Process(context.Background(), message(t, "same", 0))
	redelivery := p.Process(context.Background(), message(t, "same", 0))
	other := p.Process(context.Background(), message(t, "different", 0))

	if first != Ack || redelivery != Ack || other != Ack || len(n.calls) != 2 {
		t.Errorf("a redelivered message must be acked without a second e-mail: calls=%d", len(n.calls))
	}
}

func TestProcess_AFailedAttemptIsNotMarkedAsDone(t *testing.T) {
	n := &fakeNotifier{err: errors.New("smtp down")}
	p := NewProcessor(n, discard)

	first := p.Process(context.Background(), message(t, "m1", 0))
	n.err = nil
	second := p.Process(context.Background(), message(t, "m1", 1))

	if first != Retry || second != Ack || len(n.calls) != 2 {
		t.Errorf("first=%v second=%v calls=%d", first, second, len(n.calls))
	}
}

func TestProcess_RetriesThenDeadLetters(t *testing.T) {
	n := &fakeNotifier{err: errors.New("smtp down")}
	p := NewProcessor(n, discard)

	for retries := 0; retries < MaxRetries; retries++ {
		if got := p.Process(context.Background(), message(t, fmt.Sprint("m", retries), retries)); got != Retry {
			t.Errorf("retries=%d: action=%v, want Retry", retries, got)
		}
	}
	if got := p.Process(context.Background(), message(t, "last", MaxRetries)); got != DeadLetter {
		t.Errorf("after %d retries: action=%v, want DeadLetter", MaxRetries, got)
	}
}

func TestProcess_PermanentFailuresSkipTheRetries(t *testing.T) {
	n := &fakeNotifier{err: fmt.Errorf("bad recipient: %w", ErrPermanent)}
	p := NewProcessor(n, discard)

	if got := p.Process(context.Background(), message(t, "m1", 0)); got != DeadLetter {
		t.Errorf("action = %v, want DeadLetter", got)
	}
}

func TestProcess_PoisonMessagesGoStraightToTheDeadLetterQueue(t *testing.T) {
	n := &fakeNotifier{}
	p := NewProcessor(n, discard)

	for name, body := range map[string]string{"not json": "nope", "empty": "", "unknown shape": `{"order_id":"not-a-uuid"}`} {
		if got := p.Process(context.Background(), Message{ID: name, Body: []byte(body)}); got != DeadLetter {
			t.Errorf("%s: action = %v, want DeadLetter", name, got)
		}
	}
	if len(n.calls) != 0 {
		t.Error("a poison message must never reach the notifier")
	}
}

func TestSeenSet_ForgetsAfterTheTTLAndStaysBounded(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seen := newSeenSet(time.Hour, 3, func() time.Time { return now })

	seen.add("a")
	if !seen.has("a") {
		t.Fatal("a should be remembered")
	}
	now = now.Add(2 * time.Hour)
	if seen.has("a") {
		t.Error("a should have expired")
	}
	for _, id := range []string{"1", "2", "3", "4"} {
		seen.add(id)
	}
	if seen.size() > 3 {
		t.Errorf("size = %d, want at most 3", seen.size())
	}
	if !seen.has("4") {
		t.Error("the newest id must survive eviction")
	}
}
