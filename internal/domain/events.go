package domain

import (
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// OrderStatusChanged is fired at the end of Confirm and Cancel, only when the customer has an e-mail
// (DOMAIN.md section 5). The JSON form is the wire contract of the notification queue, shared by the API
// (producer) and the worker (consumer).
type OrderStatusChanged struct {
	OrderID       uuid.UUID       `json:"order_id"`
	CustomerEmail string          `json:"customer_email"`
	CustomerName  string          `json:"customer_name"`
	OldStatus     Status          `json:"old_status"`
	NewStatus     Status          `json:"new_status"`
	TotalAmount   decimal.Decimal `json:"total_amount"`
	ChangedAt     time.Time       `json:"changed_at"`
}

// LogValue keeps the customer's e-mail out of the logs: the order id is enough to trace the event.
func (e OrderStatusChanged) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("order_id", e.OrderID.String()),
		slog.String("old_status", string(e.OldStatus)),
		slog.String("new_status", string(e.NewStatus)),
	)
}

// LogValue logs only the id of a customer; tax id, passport number and e-mail are PII.
func (c Customer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", c.ID.String()))
}
