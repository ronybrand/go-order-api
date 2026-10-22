package postgres_test

// Integration tests: they need a real PostgreSQL (TEST_DATABASE_URL), e.g.
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/orders_test make test-integration
//
// Without the variable they are skipped, so `go test ./...` stays hermetic.

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/domain"
	"github.com/ronybrand/go-order-api/internal/postgres"
)

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func newStore(t *testing.T) *postgres.Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE order_items, orders, customers CASCADE"); err != nil {
		t.Fatal(err)
	}
	return postgres.NewStore(pool)
}

func newCustomer(t *testing.T, store *postgres.Store, taxID string, passport *string) *domain.Customer {
	t.Helper()
	c, err := domain.NewCustomer(domain.CustomerInput{Name: "Ana", TaxID: taxID, PassportNumber: passport, Email: "ana@example.com"}, "tester", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Repos().Customers.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func newOrder(t *testing.T, store *postgres.Store, customerID uuid.UUID) *domain.Order {
	t.Helper()
	o, err := domain.NewOrder(customerID, []domain.ItemInput{
		{Description: "Pen", UnitPrice: decimal.RequireFromString("2.50"), Quantity: 4},
	}, "tester", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Repos().Orders.Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestCustomers_CreateAndGet(t *testing.T) {
	store := newStore(t)
	passport := "AB123456"
	created := newCustomer(t, store, "A1234567", &passport)

	got, err := store.Repos().Customers.Get(context.Background(), created.ID)

	if err != nil {
		t.Fatal(err)
	}
	if got.ID == uuid.Nil || got.TaxID != "A1234567" || *got.PassportNumber != passport || got.Version != 1 || !got.CreatedAt.Equal(now) {
		t.Errorf("customer = %+v", got)
	}
}

func TestCustomers_UniquenessIsEnforcedByTheDatabase(t *testing.T) {
	store := newStore(t)
	passport := "AB123456"
	newCustomer(t, store, "U1234567", &passport)
	dupTax, _ := domain.NewCustomer(domain.CustomerInput{Name: "B", TaxID: "U1234567", Email: "b@example.com"}, "t", now)
	other := "ZZ999999"
	dupPassport, _ := domain.NewCustomer(domain.CustomerInput{Name: "C", TaxID: "U8765432", PassportNumber: &passport, Email: "c@example.com"}, "t", now)
	_ = other

	errTax := store.Repos().Customers.Create(context.Background(), dupTax)
	errPassport := store.Repos().Customers.Create(context.Background(), dupPassport)

	if !apperr.Is(errTax, apperr.CodeDuplicateTaxID) {
		t.Errorf("tax id err = %v", errTax)
	}
	if !apperr.Is(errPassport, apperr.CodeDuplicatePassport) {
		t.Errorf("passport err = %v", errPassport)
	}
}

func TestCustomers_SoftDeleteHidesAndReleasesUniqueness(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	c := newCustomer(t, store, "S1234567", nil)

	if err := store.Repos().Customers.SoftDelete(ctx, c.ID, "admin", now); err != nil {
		t.Fatal(err)
	}
	_, getErr := store.Repos().Customers.Get(ctx, c.ID)
	taken, err := store.Repos().Customers.TaxIDTaken(ctx, "S1234567", uuid.Nil)
	again := newCustomer(t, store, "S1234567", nil)

	if !apperr.Is(getErr, apperr.CodeCustomerNotFound) {
		t.Errorf("get after delete: %v", getErr)
	}
	if err != nil || taken {
		t.Errorf("tax id still taken after delete: %v %v", taken, err)
	}
	if again.ID == c.ID {
		t.Error("expected a new row")
	}
}

func TestCustomers_UpdateChecksTheVersion(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	c := newCustomer(t, store, "C1234567", nil)

	// Get two copies to simulate concurrent access
	stale, _ := store.Repos().Customers.Get(ctx, c.ID)
	fresh, _ := store.Repos().Customers.Get(ctx, c.ID)

	// Update fresh first
	_ = fresh.Replace(domain.CustomerInput{Name: "New", TaxID: "C1234567", Email: "n@example.com"}, "u", now)
	if err := store.Repos().Customers.Update(ctx, fresh); err != nil {
		t.Fatal(err)
	}

	// Now try to update with stale version
	_ = stale.Replace(domain.CustomerInput{Name: "Old", TaxID: "C1234567", Email: "o@example.com"}, "u", now)
	err := store.Repos().Customers.Update(ctx, stale)

	if fresh.Version != 2 {
		t.Errorf("version after update = %d", fresh.Version)
	}
	if !apperr.Is(err, apperr.CodeConcurrentModification) {
		t.Errorf("stale update err = %v", err)
	}
}

func TestOrders_RoundTripKeepsItemsInOrderAndTotalExact(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	c := newCustomer(t, store, "O1234567", nil)
	o := newOrder(t, store, c.ID)
	if err := o.AddItem(domain.ItemInput{Description: "Ink", UnitPrice: decimal.RequireFromString("0.10"), Quantity: 3}, "u", now); err != nil {
		t.Fatal(err)
	}
	if err := store.Repos().Orders.Save(ctx, o); err != nil {
		t.Fatal(err)
	}

	got, err := store.Repos().Orders.Get(ctx, o.ID)

	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 || got.Items[0].Description != "Pen" || got.Items[1].Description != "Ink" {
		t.Fatalf("items = %+v", got.Items)
	}
	if got.Items[0].ID == uuid.Nil || got.Items[1].ID == uuid.Nil || !got.Total().Equal(decimal.RequireFromString("10.30")) {
		t.Errorf("ids/total wrong: %+v total=%s", got.Items, got.Total())
	}
	if got.Version != 2 {
		t.Errorf("version = %d, want 2", got.Version)
	}
}

func TestOrders_SaveAppliesQuantityChangesAndRemovals(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	c := newCustomer(t, store, "S1123456", nil)
	o := newOrder(t, store, c.ID)
	_ = o.AddItem(domain.ItemInput{Description: "Ink", UnitPrice: decimal.RequireFromString("1"), Quantity: 1}, "u", now)
	_ = store.Repos().Orders.Save(ctx, o)
	loaded, _ := store.Repos().Orders.Get(ctx, o.ID)
	_ = loaded.UpdateItemQuantity(loaded.Items[0].ID, 9, "u", now)
	_ = loaded.RemoveItem(loaded.Items[1].ID, "u", now)

	if err := store.Repos().Orders.Save(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Repos().Orders.Get(ctx, o.ID)

	if len(got.Items) != 1 || got.Items[0].Quantity != 9 {
		t.Errorf("items = %+v", got.Items)
	}
}

func TestOrders_SaveMixesUpdatedUntouchedAndNewItemsInOneGo(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	c := newCustomer(t, store, "M1234567", nil)
	o := newOrder(t, store, c.ID) // Pen 2.50 x 4
	_ = o.AddItem(domain.ItemInput{Description: "Ink", UnitPrice: decimal.RequireFromString("1.10"), Quantity: 1}, "u", now)
	_ = o.AddItem(domain.ItemInput{Description: "Cap", UnitPrice: decimal.RequireFromString("0.05"), Quantity: 7}, "u", now)
	if err := store.Repos().Orders.Save(ctx, o); err != nil {
		t.Fatal(err)
	}
	loaded, _ := store.Repos().Orders.Get(ctx, o.ID)
	penID, inkID, capID := loaded.Items[0].ID, loaded.Items[1].ID, loaded.Items[2].ID
	_ = loaded.UpdateItemQuantity(inkID, 5, "u", now) // changed; Pen and Cap stay untouched
	_ = loaded.AddItem(domain.ItemInput{Description: "Box", UnitPrice: decimal.RequireFromString("9.99"), Quantity: 2}, "u", now)

	if err := store.Repos().Orders.Save(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Repos().Orders.Get(ctx, o.ID)

	if len(got.Items) != 4 {
		t.Fatalf("items = %+v", got.Items)
	}
	want := []struct {
		id       uuid.UUID
		desc     string
		price    string
		quantity int
	}{
		{penID, "Pen", "2.5", 4}, {inkID, "Ink", "1.1", 5}, {capID, "Cap", "0.05", 7}, {uuid.Nil, "Box", "9.99", 2},
	}
	for i, w := range want {
		item := got.Items[i]
		if (w.id != uuid.Nil && item.ID != w.id) || item.Description != w.desc || !item.UnitPrice.Equal(decimal.RequireFromString(w.price)) || item.Quantity != w.quantity {
			t.Errorf("item %d = %+v, want %+v", i, item, w)
		}
	}
	if !got.Total().Equal(decimal.RequireFromString("35.83")) { // 10.00 + 5.50 + 0.35 + 19.98
		t.Errorf("total = %s", got.Total())
	}
}

func TestOrders_SaveRejectsAStaleVersion(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	c := newCustomer(t, store, "R1234567", nil)
	o := newOrder(t, store, c.ID)
	a, _ := store.Repos().Orders.Get(ctx, o.ID)
	b, _ := store.Repos().Orders.Get(ctx, o.ID)
	_, _ = a.Confirm("u", now)
	_, _ = b.Cancel("u", now)

	errA := store.Repos().Orders.Save(ctx, a)
	errB := store.Repos().Orders.Save(ctx, b)

	if errA != nil || !apperr.Is(errB, apperr.CodeConcurrentModification) {
		t.Errorf("errA=%v errB=%v", errA, errB)
	}
	got, _ := store.Repos().Orders.Get(ctx, o.ID)
	if got.Status != domain.StatusConfirmed {
		t.Errorf("status = %s, the losing writer must not overwrite", got.Status)
	}
}

func TestOrders_HasActiveIgnoresDeletedOrders(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	c := newCustomer(t, store, "H1234567", nil)
	o := newOrder(t, store, c.ID)

	before, _ := store.Repos().Orders.HasActive(ctx, c.ID)
	_ = store.Repos().Orders.SoftDelete(ctx, o.ID, "admin", now)
	after, _ := store.Repos().Orders.HasActive(ctx, c.ID)
	_, getErr := store.Repos().Orders.Get(ctx, o.ID)

	if !before || after || !apperr.Is(getErr, apperr.CodeOrderNotFound) {
		t.Errorf("before=%v after=%v getErr=%v", before, after, getErr)
	}
}

func TestOrders_SearchFiltersSortsAndPages(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	c := newCustomer(t, store, "Q1234567", nil)
	first := newOrder(t, store, c.ID)
	second := newOrder(t, store, c.ID)
	loaded, _ := store.Repos().Orders.Get(ctx, second.ID)
	_, _ = loaded.Confirm("u", now)
	_ = store.Repos().Orders.Save(ctx, loaded)

	confirmed, err := store.Repos().Orders.Search(ctx, app.SearchRequest{Size: 10, Filters: map[string][]app.Condition{
		"status": {{Op: "eq", Value: "CONFIRMED"}}}})
	all, _ := store.Repos().Orders.Search(ctx, app.SearchRequest{Size: 1, Page: 1, Sort: "-created_at"})

	if err != nil || confirmed.Total != 1 || confirmed.Items[0].ID != second.ID || len(confirmed.Items[0].Items) != 1 {
		t.Errorf("confirmed = %+v err=%v", confirmed, err)
	}
	if all.Total != 2 || len(all.Items) != 1 {
		t.Errorf("paged = total %d items %d", all.Total, len(all.Items))
	}
	_ = first
}

func TestCustomers_SearchLikeIsAContainsMatchAndEscapesWildcards(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	newCustomer(t, store, "L1234567", nil)

	hit, err := store.Repos().Customers.Search(ctx, app.SearchRequest{Size: 10, Filters: map[string][]app.Condition{"name": {{Op: "lk", Value: "n"}}}})
	miss, _ := store.Repos().Customers.Search(ctx, app.SearchRequest{Size: 10, Filters: map[string][]app.Condition{"name": {{Op: "lk", Value: "%"}}}})

	if err != nil || hit.Total != 1 || miss.Total != 0 {
		t.Errorf("hit=%d miss=%d err=%v", hit.Total, miss.Total, err)
	}
}

func TestInTx_RollsBackOnError(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	c := newCustomer(t, store, "I1234567", nil)

	err := store.InTx(ctx, func(ctx context.Context, r app.Repositories) error {
		if _, err := domain.NewOrder(c.ID, nil, "u", now); err != nil {
			return err
		}
		o, _ := domain.NewOrder(c.ID, nil, "u", now)
		if err := r.Orders.Create(ctx, o); err != nil {
			return err
		}
		return apperr.Validation("X", "boom", nil)
	})
	page, _ := store.Repos().Orders.Search(ctx, app.SearchRequest{Size: 10})

	if !apperr.Is(err, "X") || page.Total != 0 {
		t.Errorf("err=%v orders=%d, the order must have been rolled back", err, page.Total)
	}
}

// The race the locking exists for: deleting a customer while an order is being created for it must leave
// either (order exists, delete refused) or (customer deleted, order refused), never an order under a deleted customer.
func TestCustomerDeleteVersusOrderCreate_NeverLeavesAnOrphanOrder(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	orders := app.NewOrderService(store, nil)
	customers := app.NewCustomerService(store)

	for i := 0; i < 20; i++ {
		c := newCustomer(t, store, "TAX"+uuid.NewString()[:8], nil)
		var wg sync.WaitGroup
		var createErr, deleteErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, createErr = orders.Create(ctx, c.ID, nil) }()
		go func() { defer wg.Done(); deleteErr = customers.Delete(ctx, c.ID) }()
		wg.Wait()

		if createErr == nil && deleteErr == nil {
			t.Fatalf("iteration %d: both the order was created and the customer deleted", i)
		}
	}
}
