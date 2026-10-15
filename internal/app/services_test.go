package app_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/domain"
)

var fixedNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func fixedClock() time.Time { return fixedNow }

func quiet() app.Option { return app.WithLogger(slog.New(slog.DiscardHandler)) }

func asUser(id string) context.Context {
	return app.WithActor(context.Background(), app.Actor{ID: id, Roles: []string{"user"}})
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if !apperr.Is(err, code) {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func input(taxID string) domain.CustomerInput {
	return domain.CustomerInput{Name: "Ada", TaxID: taxID, Email: "ada@example.com"}
}

func newCustomerService(store *memStore) *app.CustomerService {
	return app.NewCustomerService(store, app.WithClock(fixedClock), quiet())
}

func newOrderService(store *memStore, pub app.EventPublisher) *app.OrderService {
	return app.NewOrderService(store, pub, app.WithClock(fixedClock), quiet())
}

func widget(price string, quantity int) domain.ItemInput {
	return domain.ItemInput{Description: "Widget", UnitPrice: decimal.RequireFromString(price), Quantity: quantity}
}

// --- customers --------------------------------------------------------------------------------------

func TestCustomerService_CreateStoresAuditFromTheActor(t *testing.T) {
	store := newMemStore()

	c, err := newCustomerService(store).Create(asUser("admin-1"), input("AB123456"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == uuid.Nil || c.CreatedBy != "admin-1" || !c.CreatedAt.Equal(fixedNow) {
		t.Errorf("unexpected customer: %+v", c)
	}
}

func TestCustomerService_CreateWithoutActorRecordsTheSystem(t *testing.T) {
	c, err := newCustomerService(newMemStore()).Create(context.Background(), input("AB123456"))
	if err != nil || c.CreatedBy != app.SystemActor {
		t.Errorf("err=%v customer=%+v", err, c)
	}
}

func TestCustomerService_CreateRejectsDuplicates(t *testing.T) {
	store := newMemStore()
	svc := newCustomerService(store)
	passport := "AB123456"
	first := input("TAX-0001")
	first.PassportNumber = &passport
	if _, err := svc.Create(asUser("a"), first); err != nil {
		t.Fatal(err)
	}

	t.Run("tax id, even written with other punctuation", func(t *testing.T) {
		_, err := svc.Create(asUser("a"), input("tax0001"))
		wantCode(t, err, apperr.CodeDuplicateTaxID)
	})
	t.Run("passport number", func(t *testing.T) {
		second := input("TAX-0002")
		second.PassportNumber = &passport
		_, err := svc.Create(asUser("a"), second)
		wantCode(t, err, apperr.CodeDuplicatePassport)
	})
	t.Run("a customer without passport never conflicts on it", func(t *testing.T) {
		if _, err := svc.Create(asUser("a"), input("TAX-0003")); err != nil {
			t.Errorf("unexpected: %v", err)
		}
		if _, err := svc.Create(asUser("a"), input("TAX-0004")); err != nil {
			t.Errorf("unexpected: %v", err)
		}
	})
	t.Run("the conflict never echoes the value", func(t *testing.T) {
		_, err := svc.Create(asUser("a"), input("TAX-0001"))
		if appErr, _ := apperr.As(err); strings.Contains(appErr.Message, "TAX") || len(appErr.Params) != 0 {
			t.Errorf("leaks the tax id: %+v", appErr)
		}
	})
}

func TestCustomerService_CreateRejectsInvalidInputBeforeTouchingTheStore(t *testing.T) {
	store := newMemStore()
	in := input("AB123456")
	in.Name = ""

	_, err := newCustomerService(store).Create(asUser("a"), in)

	wantCode(t, err, apperr.CodeInvalidRequest)
	if len(store.calls) != 0 {
		t.Errorf("store was used: %v", store.calls)
	}
}

func TestCustomerService_Update(t *testing.T) {
	store := newMemStore()
	svc := newCustomerService(store)
	a, _ := svc.Create(asUser("a"), input("TAX-0001"))
	b, _ := svc.Create(asUser("a"), input("TAX-0002"))

	t.Run("keeping its own tax id is not a conflict", func(t *testing.T) {
		in := input("TAX-0001")
		in.Name = "Renamed"
		updated, err := svc.Update(asUser("editor"), a.ID, in)
		if err != nil || updated.Name != "Renamed" || updated.UpdatedBy != "editor" || updated.Version != 2 {
			t.Errorf("err=%v customer=%+v", err, updated)
		}
	})
	t.Run("taking another customer's tax id is a conflict", func(t *testing.T) {
		_, err := svc.Update(asUser("editor"), b.ID, input("TAX-0001"))
		wantCode(t, err, apperr.CodeDuplicateTaxID)
	})
	t.Run("unknown customer", func(t *testing.T) {
		_, err := svc.Update(asUser("editor"), uuid.New(), input("TAX-0009"))
		wantCode(t, err, apperr.CodeCustomerNotFound)
	})
}

func TestCustomerService_GetOfAnUnknownCustomer(t *testing.T) {
	_, err := newCustomerService(newMemStore()).Get(context.Background(), uuid.New())
	wantCode(t, err, apperr.CodeCustomerNotFound)
}

func TestCustomerService_Delete(t *testing.T) {
	t.Run("soft-deletes a customer without orders under a row lock, in one transaction", func(t *testing.T) {
		store := newMemStore()
		svc := newCustomerService(store)
		c, _ := svc.Create(asUser("a"), input("TAX-0001"))
		store.calls = nil

		if err := svc.Delete(asUser("admin"), c.ID); err != nil {
			t.Fatal(err)
		}

		want := []string{"begin", "customer.lock-update", "order.has-active", "customer.soft-delete", "commit"}
		if strings.Join(store.calls, ",") != strings.Join(want, ",") {
			t.Errorf("calls = %v, want %v", store.calls, want)
		}
		if _, err := svc.Get(context.Background(), c.ID); !apperr.Is(err, apperr.CodeCustomerNotFound) {
			t.Errorf("deleted customer is still visible: %v", err)
		}
	})
	t.Run("is blocked by an active order", func(t *testing.T) {
		store := newMemStore()
		c := store.seedCustomer(domain.Customer{Name: "Ada", TaxID: "TAX0001"})
		store.seedOrder(domain.Order{CustomerID: c.ID, Status: domain.StatusOpen})

		err := newCustomerService(store).Delete(asUser("admin"), c.ID)

		wantCode(t, err, apperr.CodeCustomerHasOrders)
		if store.lastCall() != "rollback" {
			t.Errorf("last call = %s, want rollback", store.lastCall())
		}
	})
	t.Run("a soft-deleted order does not block it", func(t *testing.T) {
		store := newMemStore()
		c := store.seedCustomer(domain.Customer{Name: "Ada", TaxID: "TAX0001"})
		deleted := fixedNow
		store.seedOrder(domain.Order{CustomerID: c.ID, Status: domain.StatusOpen, DeletedAt: &deleted})

		if err := newCustomerService(store).Delete(asUser("admin"), c.ID); err != nil {
			t.Errorf("unexpected: %v", err)
		}
	})
	t.Run("unknown customer", func(t *testing.T) {
		wantCode(t, newCustomerService(newMemStore()).Delete(asUser("admin"), uuid.New()), apperr.CodeCustomerNotFound)
	})
	t.Run("a released tax id can be registered again", func(t *testing.T) {
		store := newMemStore()
		svc := newCustomerService(store)
		c, _ := svc.Create(asUser("a"), input("TAX-0001"))
		if err := svc.Delete(asUser("a"), c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Create(asUser("a"), input("TAX-0001")); err != nil {
			t.Errorf("recreate: %v", err)
		}
	})
}

func TestSearchValidation(t *testing.T) {
	long := strings.Repeat("x", app.MaxFilterValueLength+1)
	manyFields := map[string][]app.Condition{}
	for i := range app.MaxFilterFields + 1 {
		manyFields[strings.Repeat("f", i+1)] = []app.Condition{{Op: "eq", Value: "x"}}
	}
	tests := map[string]app.SearchRequest{
		"negative page":        {Page: -1, Size: 10},
		"size above the max":   {Size: app.MaxPageSize + 1},
		"negative size":        {Size: -5},
		"offset overflow":      {Page: 2_147_483_647, Size: 100},
		"too many fields":      {Size: 10, Filters: manyFields},
		"too many conditions":  {Size: 10, Filters: map[string][]app.Condition{"name": make([]app.Condition, app.MaxConditionsPerField+1)}},
		"value too long":       {Size: 10, Filters: map[string][]app.Condition{"name": {{Op: "eq", Value: long}}}},
		"long value in a list": {Size: 10, Filters: map[string][]app.Condition{"name": {{Op: "in", Value: []any{"a", long}}}}},
		"list above the limit": {Size: 10, Filters: map[string][]app.Condition{"name": {{Op: "in", Value: make([]any, app.MaxInValues+1)}}}},
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			_, customersErr := newCustomerService(newMemStore()).Search(context.Background(), req)
			_, ordersErr := newOrderService(newMemStore(), nil).Search(context.Background(), req)
			for _, err := range []error{customersErr, ordersErr} {
				if _, ok := apperr.As(err); !ok {
					t.Errorf("err = %v, want an application error", err)
				}
			}
		})
	}
	t.Run("size defaults when omitted and limits are accepted exactly", func(t *testing.T) {
		page, err := newCustomerService(newMemStore()).Search(context.Background(), app.SearchRequest{})
		if err != nil || page.Size != app.DefaultPageSize {
			t.Errorf("err=%v page=%+v", err, page)
		}
		_, err = newCustomerService(newMemStore()).Search(context.Background(), app.SearchRequest{Size: app.MaxPageSize})
		if err != nil {
			t.Errorf("size at the limit rejected: %v", err)
		}
	})
}

// --- orders -----------------------------------------------------------------------------------------

func seedCustomerWithEmail(store *memStore, email string) *domain.Customer {
	return store.seedCustomer(domain.Customer{Name: "Ada", TaxID: "TAX0001", Email: email})
}

func TestOrderService_Create(t *testing.T) {
	t.Run("opens an order, locking the customer FOR SHARE in the same transaction", func(t *testing.T) {
		store := newMemStore()
		c := seedCustomerWithEmail(store, "ada@example.com")

		o, err := newOrderService(store, nil).Create(asUser("user-1"), c.ID, []domain.ItemInput{widget("2.50", 4)})
		if err != nil {
			t.Fatal(err)
		}
		if o.Status != domain.StatusOpen || !o.Total().Equal(decimal.NewFromInt(10)) || o.CreatedBy != "user-1" {
			t.Errorf("unexpected order: %+v", o)
		}
		want := "begin,customer.lock-share,order.create,commit"
		if got := strings.Join(store.calls, ","); got != want {
			t.Errorf("calls = %s, want %s", got, want)
		}
	})
	t.Run("a customer that does not exist is VALIDATION-07, not 404", func(t *testing.T) {
		store := newMemStore()
		_, err := newOrderService(store, nil).Create(asUser("u"), uuid.New(), nil)
		wantCode(t, err, apperr.CodeCustomerNotFoundBody)
		if store.lastCall() != "rollback" {
			t.Errorf("last call = %s", store.lastCall())
		}
	})
	t.Run("a soft-deleted customer is treated as missing", func(t *testing.T) {
		store := newMemStore()
		deleted := fixedNow
		c := store.seedCustomer(domain.Customer{Name: "Ada", TaxID: "TAX0001", DeletedAt: &deleted})
		_, err := newOrderService(store, nil).Create(asUser("u"), c.ID, nil)
		wantCode(t, err, apperr.CodeCustomerNotFoundBody)
	})
	t.Run("an invalid item stores nothing", func(t *testing.T) {
		store := newMemStore()
		c := seedCustomerWithEmail(store, "ada@example.com")
		_, err := newOrderService(store, nil).Create(asUser("u"), c.ID, []domain.ItemInput{widget("0", 1)})
		wantCode(t, err, apperr.CodeInvalidRequest)
		if len(store.data.orders) != 0 {
			t.Errorf("an order was stored: %v", store.data.orders)
		}
	})
}

func TestOrderService_ItemOperations(t *testing.T) {
	store := newMemStore()
	c := seedCustomerWithEmail(store, "ada@example.com")
	svc := newOrderService(store, nil)
	order, err := svc.Create(asUser("u"), c.ID, []domain.ItemInput{widget("10", 1)})
	if err != nil {
		t.Fatal(err)
	}

	order, err = svc.AddItem(asUser("u"), order.ID, widget("5", 2))
	if err != nil || !order.Total().Equal(decimal.NewFromInt(20)) || order.Version != 2 {
		t.Fatalf("add: err=%v order=%+v", err, order)
	}
	order, err = svc.UpdateItemQuantity(asUser("u"), order.ID, order.Items[0].ID, 3)
	if err != nil || !order.Total().Equal(decimal.NewFromInt(40)) {
		t.Fatalf("update: err=%v total=%s", err, order.Total())
	}
	order, err = svc.RemoveItem(asUser("u"), order.ID, order.Items[0].ID)
	if err != nil || !order.Total().Equal(decimal.NewFromInt(10)) || len(order.Items) != 1 {
		t.Fatalf("remove: err=%v order=%+v", err, order)
	}

	stored, _ := svc.Get(context.Background(), order.ID)
	if !stored.Total().Equal(decimal.NewFromInt(10)) {
		t.Errorf("stored total = %s", stored.Total())
	}
	_, err = svc.UpdateItemQuantity(asUser("u"), order.ID, uuid.New(), 1)
	wantCode(t, err, apperr.CodeItemNotFound)
	_, err = svc.AddItem(asUser("u"), uuid.New(), widget("1", 1))
	wantCode(t, err, apperr.CodeOrderNotFound)
}

func TestOrderService_ConfirmAndCancelPublishTheEvent(t *testing.T) {
	store := newMemStore()
	c := seedCustomerWithEmail(store, "ada@example.com")
	pub := &recordingPublisher{}
	svc := newOrderService(store, pub)
	order, _ := svc.Create(asUser("u"), c.ID, []domain.ItemInput{widget("2.50", 4)})

	confirmed, err := svc.Confirm(asUser("approver"), order.ID)
	if err != nil || confirmed.Status != domain.StatusConfirmed {
		t.Fatalf("confirm: err=%v order=%+v", err, confirmed)
	}
	if len(pub.events) != 1 {
		t.Fatalf("events = %d, want 1", len(pub.events))
	}
	e := pub.events[0]
	if e.OrderID != order.ID || e.CustomerEmail != "ada@example.com" || e.CustomerName != "Ada" ||
		e.OldStatus != domain.StatusOpen || e.NewStatus != domain.StatusConfirmed ||
		!e.TotalAmount.Equal(decimal.NewFromInt(10)) || !e.ChangedAt.Equal(fixedNow) {
		t.Errorf("unexpected event: %+v", e)
	}

	if _, err := svc.Cancel(asUser("approver"), order.ID); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 2 || pub.events[1].OldStatus != domain.StatusConfirmed || pub.events[1].NewStatus != domain.StatusCanceled {
		t.Errorf("events = %+v", pub.events)
	}
}

func TestOrderService_NoEventWhenThereIsNothingToNotify(t *testing.T) {
	t.Run("customer without e-mail", func(t *testing.T) {
		store := newMemStore()
		c := seedCustomerWithEmail(store, "")
		pub := &recordingPublisher{}
		svc := newOrderService(store, pub)
		order, _ := svc.Create(asUser("u"), c.ID, []domain.ItemInput{widget("1", 1)})
		if _, err := svc.Confirm(asUser("u"), order.ID); err != nil {
			t.Fatal(err)
		}
		if len(pub.events) != 0 {
			t.Errorf("published for a customer without e-mail: %+v", pub.events)
		}
	})
	t.Run("a failed transition publishes nothing", func(t *testing.T) {
		store := newMemStore()
		c := seedCustomerWithEmail(store, "ada@example.com")
		pub := &recordingPublisher{}
		svc := newOrderService(store, pub)
		empty, _ := svc.Create(asUser("u"), c.ID, nil)
		_, err := svc.Confirm(asUser("u"), empty.ID)
		wantCode(t, err, apperr.CodeOrderEmpty)
		if len(pub.events) != 0 {
			t.Errorf("published despite the failure: %+v", pub.events)
		}
	})
	t.Run("no publisher configured", func(t *testing.T) {
		store := newMemStore()
		c := seedCustomerWithEmail(store, "ada@example.com")
		svc := newOrderService(store, nil)
		order, _ := svc.Create(asUser("u"), c.ID, []domain.ItemInput{widget("1", 1)})
		if _, err := svc.Confirm(asUser("u"), order.ID); err != nil {
			t.Errorf("confirm without publisher: %v", err)
		}
	})
}

func TestOrderService_ABrokerFailureNeverFailsTheTransition(t *testing.T) {
	store := newMemStore()
	c := seedCustomerWithEmail(store, "ada@example.com")
	pub := &recordingPublisher{err: errors.New("broker down")}
	svc := newOrderService(store, pub)
	order, _ := svc.Create(asUser("u"), c.ID, []domain.ItemInput{widget("1", 1)})

	confirmed, err := svc.Confirm(asUser("u"), order.ID)

	if err != nil || confirmed.Status != domain.StatusConfirmed {
		t.Errorf("err=%v order=%+v", err, confirmed)
	}
	stored, _ := svc.Get(context.Background(), order.ID)
	if stored.Status != domain.StatusConfirmed {
		t.Errorf("stored status = %s", stored.Status)
	}
}

func TestOrderService_AnnounceSurvivesTheRequestContext(t *testing.T) {
	store := newMemStore()
	c := seedCustomerWithEmail(store, "ada@example.com")
	pub := &ctxPublisher{}
	svc := newOrderService(store, pub)
	order, _ := svc.Create(asUser("u"), c.ID, []domain.ItemInput{widget("1", 1)})
	// The client hangs up: by the time the commit is done the request context may already be canceled.
	ctx, cancel := context.WithCancel(asUser("u"))
	cancel()

	if _, err := svc.Confirm(ctx, order.ID); err != nil {
		t.Fatal(err)
	}

	if pub.calls != 1 {
		t.Fatalf("publish calls = %d, want 1", pub.calls)
	}
	if pub.ctxErr != nil {
		t.Errorf("publisher got a canceled context: %v", pub.ctxErr)
	}
	if !pub.hasDeadline {
		t.Error("publisher got a context without a deadline")
	}
	if got := pub.actor.ID; got != "u" {
		t.Errorf("actor lost from the publish context: %q", got)
	}
}

func TestOrderService_AnnounceLogsWhyItCouldNotLoadTheCustomer(t *testing.T) {
	store := newMemStore()
	c := seedCustomerWithEmail(store, "ada@example.com")
	pub := &recordingPublisher{}
	var logs bytes.Buffer
	svc := app.NewOrderService(store, pub, app.WithClock(fixedClock), app.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	order, _ := svc.Create(asUser("u"), c.ID, []domain.ItemInput{widget("1", 1)})
	if err := store.Repos().Customers.SoftDelete(context.Background(), c.ID, "x", fixedNow); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Confirm(asUser("u"), order.ID); err != nil {
		t.Fatalf("the transition must not fail: %v", err)
	}

	if len(pub.events) != 0 {
		t.Errorf("published without a customer: %+v", pub.events)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || strings.Contains(logs.String(), "ada@example.com") {
		t.Errorf("expected an error log without PII, got: %s", logs.String())
	}
}

func TestOrderService_RuleViolationsKeepTheirCodes(t *testing.T) {
	store := newMemStore()
	c := seedCustomerWithEmail(store, "ada@example.com")
	svc := newOrderService(store, nil)
	order, _ := svc.Create(asUser("u"), c.ID, []domain.ItemInput{widget("1", 1)})
	if _, err := svc.Confirm(asUser("u"), order.ID); err != nil {
		t.Fatal(err)
	}

	_, err := svc.Confirm(asUser("u"), order.ID)
	wantCode(t, err, apperr.CodeInvalidTransition)
	_, err = svc.AddItem(asUser("u"), order.ID, widget("1", 1))
	wantCode(t, err, apperr.CodeOrderNotEditable)
	if _, err := svc.Cancel(asUser("u"), order.ID); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Cancel(asUser("u"), order.ID)
	wantCode(t, err, apperr.CodeAlreadyCanceled)
}

func TestOrderService_ConcurrentModificationIsAConflictNotAnOverwrite(t *testing.T) {
	store := newMemStore()
	c := seedCustomerWithEmail(store, "ada@example.com")
	svc := newOrderService(store, nil)
	order, _ := svc.Create(asUser("u"), c.ID, []domain.ItemInput{widget("1", 1)})
	// Someone else commits a change to the same order between our read and our save.
	store.beforeOrderSave = func(d *memData) { d.orders[order.ID].Version++ }

	_, err := svc.AddItem(asUser("u"), order.ID, widget("1", 1))

	wantCode(t, err, apperr.CodeConcurrentModification)
	if stored := store.data.orders[order.ID]; len(stored.Items) != 1 {
		t.Errorf("the losing write was applied: %+v", stored.Items)
	}
}

func TestOrderService_Delete(t *testing.T) {
	store := newMemStore()
	c := seedCustomerWithEmail(store, "ada@example.com")
	svc := newOrderService(store, nil)
	order, _ := svc.Create(asUser("u"), c.ID, nil)

	if err := svc.Delete(asUser("u"), order.ID); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Get(context.Background(), order.ID)
	wantCode(t, err, apperr.CodeOrderNotFound)
	wantCode(t, svc.Delete(asUser("u"), order.ID), apperr.CodeOrderNotFound)
}

func TestActorFrom(t *testing.T) {
	if got := app.ActorFrom(context.Background()); got.ID != app.SystemActor {
		t.Errorf("no actor = %+v", got)
	}
	if got := app.ActorFrom(app.WithActor(context.Background(), app.Actor{})); got.ID != app.SystemActor {
		t.Errorf("empty actor = %+v", got)
	}
	if got := app.ActorFrom(asUser("u-1")); got.ID != "u-1" {
		t.Errorf("actor = %+v", got)
	}
}
