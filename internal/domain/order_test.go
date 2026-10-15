package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/domain"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func item(price string, quantity int) domain.ItemInput {
	return domain.ItemInput{Description: "Widget", UnitPrice: dec(price), Quantity: quantity}
}

// openOrder builds an OPEN order whose items already have ids, as if loaded from the database.
func openOrder(t *testing.T, items ...domain.ItemInput) *domain.Order {
	t.Helper()
	o, err := domain.NewOrder(uuid.New(), items, "user-1", now)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	o.ID = uuid.New()
	for i := range o.Items {
		o.Items[i].ID = uuid.New()
	}
	return o
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if !apperr.Is(err, code) {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func TestNewOrder_BuildsAnOpenOrderWithTheTotal(t *testing.T) {
	o := openOrder(t, item("2.50", 4), item("10", 1))

	if o.Status != domain.StatusOpen || o.Version != 1 || len(o.Items) != 2 {
		t.Errorf("unexpected order: %+v", o)
	}
	if got := o.Total(); !got.Equal(dec("20")) {
		t.Errorf("Total = %s, want 20", got)
	}
}

func TestOrder_TotalIsAlwaysDerivedFromTheCurrentItems(t *testing.T) {
	o := openOrder(t, item("10", 2))
	if err := o.AddItem(item("0.10", 3), "u", now); err != nil {
		t.Fatal(err)
	}
	if got := o.Total(); !got.Equal(dec("20.30")) {
		t.Errorf("after add: %s", got)
	}
	if err := o.UpdateItemQuantity(o.Items[0].ID, 1, "u", now); err != nil {
		t.Fatal(err)
	}
	if got := o.Total(); !got.Equal(dec("10.30")) {
		t.Errorf("after update: %s", got)
	}
	if err := o.RemoveItem(o.Items[0].ID, "u", now); err != nil {
		t.Fatal(err)
	}
	if got := o.Total(); !got.Equal(dec("0.30")) {
		t.Errorf("after remove: %s", got)
	}
}

func TestOrder_TotalDoesNotLoseCents(t *testing.T) {
	// 0.1 + 0.2 in binary floating point is 0.30000000000000004.
	o := openOrder(t, item("0.10", 1), item("0.20", 1))
	if got := o.Total().String(); got != "0.3" {
		t.Errorf("Total = %s", got)
	}
}

func TestNewOrder_RejectsInvalidItems(t *testing.T) {
	tests := map[string]domain.ItemInput{
		"blank description":  {Description: "  ", UnitPrice: dec("1"), Quantity: 1},
		"long description":   {Description: strings.Repeat("a", 256), UnitPrice: dec("1"), Quantity: 1},
		"zero price":         item("0", 1),
		"negative price":     item("-1", 1),
		"three decimals":     item("1.001", 1),
		"price above column": item("100000000", 1),
		"zero quantity":      item("1", 0),
		"negative quantity":  item("1", -3),
		"quantity above max": item("1", domain.MaxQuantity+1),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := domain.NewOrder(uuid.New(), []domain.ItemInput{in}, "u", now)
			wantCode(t, err, apperr.CodeInvalidRequest)
		})
	}
}

func TestNewOrder_ItemLimitIsExact(t *testing.T) {
	at := make([]domain.ItemInput, domain.MaxItemsPerOrder)
	for i := range at {
		at[i] = item("1", 1)
	}
	if _, err := domain.NewOrder(uuid.New(), at, "u", now); err != nil {
		t.Fatalf("200 items must be accepted: %v", err)
	}
	_, err := domain.NewOrder(uuid.New(), append(at, item("1", 1)), "u", now)
	wantCode(t, err, apperr.CodeItemLimit)
}

func TestOrder_AddItemRespectsTheLimitToo(t *testing.T) {
	o := openOrder(t)
	for range domain.MaxItemsPerOrder {
		if err := o.AddItem(item("1", 1), "u", now); err != nil {
			t.Fatal(err)
		}
	}
	wantCode(t, o.AddItem(item("1", 1), "u", now), apperr.CodeItemLimit)
}

func TestOrder_ItemsAreEditableOnlyWhileOpen(t *testing.T) {
	for _, status := range []domain.Status{domain.StatusConfirmed, domain.StatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			o := openOrder(t, item("1", 1))
			o.Status = status

			wantCode(t, o.AddItem(item("1", 1), "u", now), apperr.CodeOrderNotEditable)
			wantCode(t, o.UpdateItemQuantity(o.Items[0].ID, 2, "u", now), apperr.CodeOrderNotEditable)
			wantCode(t, o.RemoveItem(o.Items[0].ID, "u", now), apperr.CodeOrderNotEditable)
			if len(o.Items) != 1 || o.Items[0].Quantity != 1 {
				t.Errorf("items changed on a %s order: %+v", status, o.Items)
			}
		})
	}
}

func TestOrder_ItemOperationsOnAnUnknownItem(t *testing.T) {
	o := openOrder(t, item("1", 1))
	unknown := uuid.New()

	wantCode(t, o.UpdateItemQuantity(unknown, 2, "u", now), apperr.CodeItemNotFound)
	wantCode(t, o.RemoveItem(unknown, "u", now), apperr.CodeItemNotFound)
}

func TestOrder_UpdateItemQuantityValidatesTheQuantity(t *testing.T) {
	o := openOrder(t, item("1", 1))
	wantCode(t, o.UpdateItemQuantity(o.Items[0].ID, 0, "u", now), apperr.CodeInvalidRequest)
	wantCode(t, o.UpdateItemQuantity(o.Items[0].ID, domain.MaxQuantity+1, "u", now), apperr.CodeInvalidRequest)
	if err := o.UpdateItemQuantity(o.Items[0].ID, domain.MaxQuantity, "u", now); err != nil {
		t.Errorf("quantity at the limit rejected: %v", err)
	}
}

func TestOrder_Confirm(t *testing.T) {
	t.Run("moves OPEN to CONFIRMED and reports the previous status", func(t *testing.T) {
		o := openOrder(t, item("1", 1))
		old, err := o.Confirm("approver", now.Add(time.Minute))
		if err != nil || old != domain.StatusOpen || o.Status != domain.StatusConfirmed || o.UpdatedBy != "approver" {
			t.Errorf("old=%s err=%v order=%+v", old, err, o)
		}
	})
	t.Run("an empty order cannot be confirmed", func(t *testing.T) {
		o := openOrder(t)
		_, err := o.Confirm("u", now)
		wantCode(t, err, apperr.CodeOrderEmpty)
		if o.Status != domain.StatusOpen {
			t.Errorf("status changed to %s", o.Status)
		}
	})
	for _, status := range []domain.Status{domain.StatusConfirmed, domain.StatusCanceled} {
		t.Run("not from "+string(status), func(t *testing.T) {
			o := openOrder(t, item("1", 1))
			o.Status = status
			_, err := o.Confirm("u", now)
			wantCode(t, err, apperr.CodeInvalidTransition)
		})
	}
}

func TestOrder_Cancel(t *testing.T) {
	for _, from := range []domain.Status{domain.StatusOpen, domain.StatusConfirmed} {
		t.Run("from "+string(from), func(t *testing.T) {
			o := openOrder(t, item("1", 1))
			o.Status = from
			old, err := o.Cancel("u", now)
			if err != nil || old != from || o.Status != domain.StatusCanceled {
				t.Errorf("old=%s err=%v status=%s", old, err, o.Status)
			}
		})
	}
	t.Run("an already canceled order cannot be canceled again", func(t *testing.T) {
		o := openOrder(t, item("1", 1))
		o.Status = domain.StatusCanceled
		_, err := o.Cancel("u", now)
		wantCode(t, err, apperr.CodeAlreadyCanceled)
	})
}

func TestStatus_Valid(t *testing.T) {
	for _, s := range []domain.Status{domain.StatusOpen, domain.StatusConfirmed, domain.StatusCanceled} {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	for _, s := range []domain.Status{"", "open", "SHIPPED"} {
		if s.Valid() {
			t.Errorf("%q should be invalid", s)
		}
	}
}
