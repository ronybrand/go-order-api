package domain

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ronybrand/go-order-api/internal/apperr"
)

// Order rules (DOMAIN.md section 4).
const (
	MaxItemsPerOrder         = 200
	MaxItemDescription       = 255
	MaxQuantity              = 10_000
	moneyScale         int32 = 2
)

// MaxUnitPrice is the largest value the numeric(10,2) column holds.
var MaxUnitPrice = decimal.RequireFromString("99999999.99")

// Status is the lifecycle state of an order: OPEN -> CONFIRMED -> CANCELED, or OPEN -> CANCELED.
type Status string

// The order statuses.
const (
	StatusOpen      Status = "OPEN"
	StatusConfirmed Status = "CONFIRMED"
	StatusCanceled  Status = "CANCELED"
)

// Valid reports whether s is one of the known statuses.
func (s Status) Valid() bool {
	return s == StatusOpen || s == StatusConfirmed || s == StatusCanceled
}

// Item is an order line. Its subtotal is derived, never stored.
type Item struct {
	ID          uuid.UUID
	OrderID     uuid.UUID
	Description string
	UnitPrice   decimal.Decimal
	Quantity    int
}

// Subtotal is unit price times quantity.
func (i Item) Subtotal() decimal.Decimal {
	return i.UnitPrice.Mul(decimal.NewFromInt(int64(i.Quantity)))
}

// ItemInput is the data a client supplies for a new item.
type ItemInput struct {
	Description string
	UnitPrice   decimal.Decimal
	Quantity    int
}

func (in ItemInput) validate() error {
	switch {
	case strings.TrimSpace(in.Description) == "":
		return apperr.InvalidField("description", "description must not be blank")
	case utf8.RuneCountInString(in.Description) > MaxItemDescription:
		return apperr.InvalidField("description", "description is too long")
	case !in.UnitPrice.IsPositive():
		return apperr.InvalidField("unit_price", "unit_price must be greater than zero")
	case in.UnitPrice.GreaterThan(MaxUnitPrice):
		return apperr.InvalidField("unit_price", "unit_price is too large")
	case !in.UnitPrice.Round(moneyScale).Equal(in.UnitPrice):
		return apperr.InvalidField("unit_price", "unit_price must have at most 2 decimal places")
	}
	return validQuantity(in.Quantity)
}

func validQuantity(quantity int) error {
	if quantity < 1 || quantity > MaxQuantity {
		return apperr.InvalidField("quantity", "quantity must be between 1 and 10000")
	}
	return nil
}

// Order is the aggregate root: it owns its items and keeps the total consistent with them.
type Order struct {
	ID         uuid.UUID
	CustomerID uuid.UUID
	Status     Status
	Items      []Item
	// Version is the optimistic-concurrency token: a write that changes status or items must carry the
	// version it read, and the repository fails with CONFLICT-00 if the row moved on.
	Version int64

	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy string
	UpdatedBy string
	DeletedAt *time.Time
	DeletedBy *string
}

// NewOrder builds an OPEN order from the requested items (rule 5 and 6).
func NewOrder(customerID uuid.UUID, items []ItemInput, actor string, now time.Time) (*Order, error) {
	if len(items) > MaxItemsPerOrder {
		return nil, itemLimitError()
	}
	o := &Order{
		CustomerID: customerID, Status: StatusOpen, Version: 1,
		CreatedAt: now, UpdatedAt: now, CreatedBy: actor, UpdatedBy: actor,
	}
	for _, in := range items {
		if err := in.validate(); err != nil {
			return nil, err
		}
		o.Items = append(o.Items, Item{Description: in.Description, UnitPrice: in.UnitPrice, Quantity: in.Quantity})
	}
	return o, nil
}

// Total is derived from the current items on every call (rule 1): there is no stored total to go stale.
func (o *Order) Total() decimal.Decimal {
	total := decimal.Zero
	for _, item := range o.Items {
		total = total.Add(item.Subtotal())
	}
	return total.Round(moneyScale)
}

// Editable reports whether items may still change (rule 2).
func (o *Order) Editable() bool {
	return o.Status == StatusOpen
}

// AddItem appends an item to an OPEN order.
func (o *Order) AddItem(in ItemInput, actor string, now time.Time) error {
	if err := o.requireEditable(); err != nil {
		return err
	}
	if len(o.Items) >= MaxItemsPerOrder {
		return itemLimitError()
	}
	if err := in.validate(); err != nil {
		return err
	}
	o.Items = append(o.Items, Item{OrderID: o.ID, Description: in.Description, UnitPrice: in.UnitPrice, Quantity: in.Quantity})
	o.touch(actor, now)
	return nil
}

// UpdateItemQuantity changes the quantity of one item of an OPEN order.
func (o *Order) UpdateItemQuantity(itemID uuid.UUID, quantity int, actor string, now time.Time) error {
	if err := o.requireEditable(); err != nil {
		return err
	}
	index, err := o.itemIndex(itemID)
	if err != nil {
		return err
	}
	if err := validQuantity(quantity); err != nil {
		return err
	}
	o.Items[index].Quantity = quantity
	o.touch(actor, now)
	return nil
}

// RemoveItem drops one item of an OPEN order.
func (o *Order) RemoveItem(itemID uuid.UUID, actor string, now time.Time) error {
	if err := o.requireEditable(); err != nil {
		return err
	}
	index, err := o.itemIndex(itemID)
	if err != nil {
		return err
	}
	o.Items = append(o.Items[:index], o.Items[index+1:]...)
	o.touch(actor, now)
	return nil
}

// Confirm moves OPEN -> CONFIRMED (rule 3) and returns the previous status.
func (o *Order) Confirm(actor string, now time.Time) (Status, error) {
	if o.Status != StatusOpen {
		return o.Status, apperr.Validation(apperr.CodeInvalidTransition,
			"cannot confirm an order that is not OPEN", map[string]any{"id": o.ID, "status": o.Status})
	}
	if len(o.Items) == 0 {
		return o.Status, apperr.Validation(apperr.CodeOrderEmpty,
			"an order without items cannot be confirmed", map[string]any{"id": o.ID})
	}
	return o.transition(StatusConfirmed, actor, now), nil
}

// Cancel moves OPEN|CONFIRMED -> CANCELED (rule 4) and returns the previous status.
func (o *Order) Cancel(actor string, now time.Time) (Status, error) {
	if o.Status == StatusCanceled {
		return o.Status, apperr.Validation(apperr.CodeAlreadyCanceled,
			"order is already canceled", map[string]any{"id": o.ID, "status": o.Status})
	}
	return o.transition(StatusCanceled, actor, now), nil
}

func (o *Order) transition(to Status, actor string, now time.Time) Status {
	from := o.Status
	o.Status = to
	o.touch(actor, now)
	return from
}

func (o *Order) touch(actor string, now time.Time) {
	o.UpdatedBy, o.UpdatedAt = actor, now
}

func (o *Order) requireEditable() error {
	if !o.Editable() {
		return apperr.Validation(apperr.CodeOrderNotEditable,
			"order is not editable in its current status", map[string]any{"id": o.ID, "status": o.Status})
	}
	return nil
}

func (o *Order) itemIndex(itemID uuid.UUID) (int, error) {
	for i, item := range o.Items {
		if item.ID == itemID {
			return i, nil
		}
	}
	return 0, apperr.NotFound(apperr.CodeItemNotFound, "item not found in this order",
		map[string]any{"order_id": o.ID, "item_id": itemID})
}

func itemLimitError() error {
	return apperr.Validation(apperr.CodeItemLimit, "an order can have at most 200 items",
		map[string]any{"max": MaxItemsPerOrder})
}
