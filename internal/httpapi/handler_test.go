package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/auth"
	"github.com/ronybrand/go-order-api/internal/domain"
)

var testNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type fakeCustomers struct {
	created  domain.CustomerInput
	search   app.SearchRequest
	err      error
	customer *domain.Customer
}

func (f *fakeCustomers) Create(_ context.Context, in domain.CustomerInput) (*domain.Customer, error) {
	f.created = in
	return f.customer, f.err
}
func (f *fakeCustomers) Get(context.Context, uuid.UUID) (*domain.Customer, error) {
	return f.customer, f.err
}
func (f *fakeCustomers) Update(_ context.Context, _ uuid.UUID, in domain.CustomerInput) (*domain.Customer, error) {
	f.created = in
	return f.customer, f.err
}
func (f *fakeCustomers) Delete(context.Context, uuid.UUID) error { return f.err }
func (f *fakeCustomers) Search(_ context.Context, req app.SearchRequest) (app.Page[domain.Customer], error) {
	f.search = req
	return app.Page[domain.Customer]{Items: []domain.Customer{*f.customer}, Total: 1, Page: req.Page, Size: req.Size}, f.err
}

type fakeOrders struct {
	order      *domain.Order
	err        error
	createdFor uuid.UUID
	items      []domain.ItemInput
	quantity   int
	search     app.SearchRequest
}

func (f *fakeOrders) Create(_ context.Context, customerID uuid.UUID, items []domain.ItemInput) (*domain.Order, error) {
	f.createdFor, f.items = customerID, items
	return f.order, f.err
}
func (f *fakeOrders) Get(context.Context, uuid.UUID) (*domain.Order, error) { return f.order, f.err }
func (f *fakeOrders) Delete(context.Context, uuid.UUID) error               { return f.err }
func (f *fakeOrders) AddItem(_ context.Context, _ uuid.UUID, in domain.ItemInput) (*domain.Order, error) {
	f.items = []domain.ItemInput{in}
	return f.order, f.err
}
func (f *fakeOrders) UpdateItemQuantity(_ context.Context, _, _ uuid.UUID, q int) (*domain.Order, error) {
	f.quantity = q
	return f.order, f.err
}
func (f *fakeOrders) RemoveItem(context.Context, uuid.UUID, uuid.UUID) (*domain.Order, error) {
	return f.order, f.err
}
func (f *fakeOrders) Confirm(context.Context, uuid.UUID) (*domain.Order, error) {
	return f.order, f.err
}
func (f *fakeOrders) Cancel(context.Context, uuid.UUID) (*domain.Order, error) { return f.order, f.err }
func (f *fakeOrders) Search(_ context.Context, req app.SearchRequest) (app.Page[domain.Order], error) {
	f.search = req
	return app.Page[domain.Order]{Items: []domain.Order{*f.order}, Total: 1, Page: req.Page, Size: req.Size}, f.err
}

type harness struct {
	handler   http.Handler
	customers *fakeCustomers
	orders    *fakeOrders
	healthErr error
}

func newHarness(t *testing.T, mutate ...func(*Options)) *harness {
	t.Helper()
	itemID := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	orderID := uuid.MustParse("00000000-0000-0000-0000-0000000000bb")
	h := &harness{
		customers: &fakeCustomers{customer: &domain.Customer{
			ID: uuid.MustParse("00000000-0000-0000-0000-0000000000cc"), Name: "Ana", TaxID: "12345678", Email: "ana@example.com", Version: 1}},
		orders: &fakeOrders{order: &domain.Order{
			ID: orderID, CustomerID: uuid.MustParse("00000000-0000-0000-0000-0000000000cc"), Status: domain.StatusOpen, Version: 1,
			Items: []domain.Item{{ID: itemID, OrderID: orderID, Description: "Pen", UnitPrice: decimal.RequireFromString("2.50"), Quantity: 4}}}},
	}
	opts := Options{
		Customers: h.customers, Orders: h.orders, Logger: discard, MaxBodyBytes: 1 << 20,
		RateLimitPerUser: 1000, RateLimitWrites: 1000, RateLimitPerIP: 1000, Now: func() time.Time { return testNow },
		Verifier: fakeVerifier{
			"user":  {Subject: "u1", Roles: []string{auth.RoleUser}},
			"admin": {Subject: "a1", Roles: []string{auth.RoleAdmin}},
		},
		Ping: func(context.Context) error { return h.healthErr },
	}
	for _, m := range mutate {
		m(&opts)
	}
	h.handler = New(opts)
	return h
}

func (h *harness) call(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return do(h.handler, req)
}

const (
	orderPath = "/orders/00000000-0000-0000-0000-0000000000bb"
	itemPath  = orderPath + "/items/00000000-0000-0000-0000-0000000000aa"
	custPath  = "/customers/00000000-0000-0000-0000-0000000000cc"
)

func TestHealth_IsPublicAndReportsTheDatabase(t *testing.T) {
	h := newHarness(t)

	up := h.call("GET", "/health", "", "")
	h.healthErr = context.DeadlineExceeded
	down := h.call("GET", "/health", "", "")

	if up.Code != 200 || down.Code != 503 || strings.Contains(down.Body.String(), "deadline") {
		t.Errorf("up=%d down=%d %s", up.Code, down.Code, down.Body)
	}
}

func TestEveryProtectedRouteRequiresAuthentication(t *testing.T) {
	h := newHarness(t)
	routes := []string{
		"POST /orders", "GET /orders/search", "POST /orders/search", "GET " + orderPath, "DELETE " + orderPath,
		"POST " + orderPath + "/items", "PATCH " + itemPath, "DELETE " + itemPath,
		"POST " + orderPath + "/confirm", "POST " + orderPath + "/cancel",
		"POST /customers", "GET /customers/search", "POST /customers/search", "GET " + custPath, "PUT " + custPath, "DELETE " + custPath,
	}
	for _, route := range routes {
		method, path, _ := strings.Cut(route, " ")
		t.Run(route, func(t *testing.T) {
			rec := h.call(method, path, "", "{}")

			if rec.Code != 401 || errorBody(t, rec)["code"] != "AUTH-01" {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestCustomerMutationsRequireTheAdminRole(t *testing.T) {
	h := newHarness(t)
	body := `{"name":"Ana","tax_id":"12345678","email":"ana@example.com"}`

	for _, route := range []string{"POST /customers", "PUT " + custPath, "DELETE " + custPath} {
		method, path, _ := strings.Cut(route, " ")
		if rec := h.call(method, path, "user", body); rec.Code != 403 || errorBody(t, rec)["code"] != "AUTH-02" {
			t.Errorf("%s as user: %d %s", route, rec.Code, rec.Body)
		}
	}
	// Reads stay open to any authenticated user.
	if rec := h.call("GET", custPath, "user", ""); rec.Code != 200 {
		t.Errorf("GET customer as user: %d", rec.Code)
	}
}

func TestCreateCustomer_ReturnsOnlyIDAndName(t *testing.T) {
	h := newHarness(t)

	rec := h.call("POST", "/customers", "admin", `{"name":"Ana","tax_id":"123.456-78","passport_number":"AB123456","email":"ana@example.com"}`)

	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body) != 2 || body["name"] != "Ana" || body["id"] == "" {
		t.Errorf("customer response must expose only id and name (PII stays out): %v", body)
	}
	if h.customers.created.TaxID != "123.456-78" || *h.customers.created.PassportNumber != "AB123456" {
		t.Errorf("input not forwarded: %+v", h.customers.created)
	}
}

func TestCreateCustomer_RejectsUnknownFieldsAndMalformedJSON(t *testing.T) {
	h := newHarness(t)

	for _, body := range []string{`{"name":"Ana","tax_id":"12345678","email":"a@b.co","admin":true}`, `{`, ``} {
		if rec := h.call("POST", "/customers", "admin", body); rec.Code != 400 || errorBody(t, rec)["code"] != "VALIDATION-01" {
			t.Errorf("body %q: %d %s", body, rec.Code, rec.Body)
		}
	}
}

func TestDeleteCustomer_AnswersNoContentAndPropagatesConflicts(t *testing.T) {
	h := newHarness(t)

	ok := h.call("DELETE", custPath, "admin", "")
	h.customers.err = apperr.Conflict(apperr.CodeCustomerHasOrders, "has orders", nil)
	conflict := h.call("DELETE", custPath, "admin", "")

	if ok.Code != 204 || ok.Body.Len() != 0 || conflict.Code != 409 || errorBody(t, conflict)["code"] != "CONFLICT-03" {
		t.Errorf("ok=%d conflict=%d %s", ok.Code, conflict.Code, conflict.Body)
	}
}

func TestCreateOrder_AcceptsNumbersWithoutLosingPrecision(t *testing.T) {
	h := newHarness(t)

	rec := h.call("POST", "/orders", "user", `{"customer_id":"00000000-0000-0000-0000-0000000000cc",
		"items":[{"description":"Pen","unit_price":0.10,"quantity":3}]}`)

	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(h.orders.items) != 1 || !h.orders.items[0].UnitPrice.Equal(decimal.RequireFromString("0.10")) || h.orders.items[0].Quantity != 3 {
		t.Errorf("items = %+v", h.orders.items)
	}
}

func TestOrderResponse_ShapeAndDerivedAmounts(t *testing.T) {
	h := newHarness(t)

	rec := h.call("GET", orderPath, "user", "")

	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var body struct {
		Status string      `json:"status"`
		Total  json.Number `json:"total"`
		Items  []struct {
			UnitPrice json.Number `json:"unit_price"`
			Subtotal  json.Number `json:"subtotal"`
		} `json:"items"`
	}
	raw := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "OPEN" || body.Total != "10.00" || len(body.Items) != 1 || body.Items[0].UnitPrice != "2.50" || body.Items[0].Subtotal != "10.00" {
		t.Errorf("body = %+v raw=%v", body, raw)
	}
	if _, isNumber := raw["total"].(float64); !isNumber {
		t.Errorf("total must be a JSON number, got %T", raw["total"])
	}
}

func TestOrderRoutes_MapToServiceCalls(t *testing.T) {
	h := newHarness(t)

	tests := map[string]struct {
		method, path, body string
		status             int
	}{
		"add item":        {"POST", orderPath + "/items", `{"description":"Ink","unit_price":1.5,"quantity":2}`, 200},
		"update quantity": {"PATCH", itemPath, `{"quantity":7}`, 200},
		"remove item":     {"DELETE", itemPath, "", 200},
		"confirm":         {"POST", orderPath + "/confirm", "", 200},
		"cancel":          {"POST", orderPath + "/cancel", "", 200},
		"delete order":    {"DELETE", orderPath, "", 204},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if rec := h.call(tt.method, tt.path, "user", tt.body); rec.Code != tt.status {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	if h.orders.quantity != 7 {
		t.Errorf("quantity = %d", h.orders.quantity)
	}
}

func TestPathIDsMustBeUUIDs(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/orders/not-a-uuid", "/customers/123", "/orders/" + "00000000-0000-0000-0000-0000000000bb" + "/items/xyz"} {
		method := "GET"
		if strings.Contains(path, "/items/") {
			method = "DELETE"
		}
		if rec := h.call(method, path, "user", ""); rec.Code != 400 || errorBody(t, rec)["code"] != "VALIDATION-01" {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
}

func TestServiceErrorsKeepTheirStatus(t *testing.T) {
	h := newHarness(t)
	h.orders.err = apperr.NotFound(apperr.CodeOrderNotFound, "order not found", map[string]any{"id": "x"})

	rec := h.call("GET", orderPath, "user", "")

	if rec.Code != 404 || errorBody(t, rec)["code"] != "RESOURCE-NOT-FOUND-02" {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestSearchGET_ParsesPageSizeAndSortOnly(t *testing.T) {
	h := newHarness(t)

	rec := h.call("GET", "/orders/search?page=2&size=5&sort=-created_at&status=OPEN", "user", "")

	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := h.orders.search
	if got.Page != 2 || got.Size != 5 || got.Sort != "-created_at" || len(got.Filters) != 0 {
		t.Errorf("search = %+v", got)
	}
	var page struct {
		Items             []any
		Total, Page, Size int
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.Total != 1 || page.Page != 2 || page.Size != 5 {
		t.Errorf("page = %+v", page)
	}
}

func TestSearchGET_RejectsNonNumericPaging(t *testing.T) {
	h := newHarness(t)

	for _, query := range []string{"page=abc", "size=1.5", "size=99999999999999999999"} {
		if rec := h.call("GET", "/customers/search?"+query, "user", ""); rec.Code != 400 || errorBody(t, rec)["code"] != "VALIDATION-01" {
			t.Errorf("%s: %d %s", query, rec.Code, rec.Body)
		}
	}
}

func TestSearchPOST_ForwardsFilters(t *testing.T) {
	h := newHarness(t)

	rec := h.call("POST", "/orders/search", "user", `{"page":0,"size":10,"sort":"version",
		"filters":{"status":[{"op":"in","value":["OPEN","CONFIRMED"]}],"version":[{"op":"gte","value":2}]}}`)

	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := h.orders.search
	if got.Sort != "version" || len(got.Filters["status"]) != 1 || got.Filters["status"][0].Op != "in" || got.Filters["version"][0].Value != json.Number("2") {
		t.Errorf("search = %+v", got)
	}
}

func TestWriteBudget_IsSeparateFromTheGeneralBudget(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RateLimitWrites = 2; o.RateLimitPerUser = 100 })

	var codes []int
	for range 3 {
		codes = append(codes, h.call("POST", orderPath+"/confirm", "user", "").Code)
	}
	read := h.call("GET", orderPath, "user", "")
	otherUser := h.call("POST", orderPath+"/confirm", "admin", "")

	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Errorf("write codes = %v", codes)
	}
	if read.Code != 200 || otherUser.Code != 200 {
		t.Errorf("reads and other users must not share the exhausted write budget: %d %d", read.Code, otherUser.Code)
	}
}

func TestHealthIsExemptFromRateLimits(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RateLimitPerIP = 1 })

	for range 5 {
		if rec := h.call("GET", "/health", "", ""); rec.Code != 200 {
			t.Fatalf("health limited: %d", rec.Code)
		}
	}
}

func TestIPLimitAppliesBeforeAuthentication(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RateLimitPerIP = 2 })

	var codes []int
	for range 3 {
		codes = append(codes, h.call("GET", orderPath, "", "").Code)
	}

	if codes[0] != 401 || codes[1] != 401 || codes[2] != 429 {
		t.Errorf("codes = %v", codes)
	}
}

func TestResponsesCarryTheBaselineHeaders(t *testing.T) {
	h := newHarness(t)

	rec := h.call("GET", "/health", "", "")

	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Request-Id") == "" {
		t.Errorf("headers = %v", rec.Header())
	}
}

func TestUnknownRouteAndMethodUseTheErrorEnvelope(t *testing.T) {
	h := newHarness(t)

	notFound := h.call("GET", "/nope", "user", "")
	wrongMethod := h.call("PUT", "/orders", "user", "{}")

	for _, rec := range []*httptest.ResponseRecorder{notFound, wrongMethod} {
		if errorBody(t, rec)["request_id"] == nil || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	}
	if notFound.Code != 404 || wrongMethod.Code != 405 {
		t.Errorf("statuses = %d %d", notFound.Code, wrongMethod.Code)
	}
}

func TestAccessLog_RecordsARequestThatPanicked(t *testing.T) {
	var logs bytes.Buffer
	h := newHarness(t, func(o *Options) {
		o.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
		o.Ping = func(context.Context) error { panic("boom") }
	})

	rec := h.call(http.MethodGet, "/health", "", "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	var access map[string]any
	for line := range bytes.SplitSeq(logs.Bytes(), []byte{'\n'}) {
		var entry map[string]any
		if json.Unmarshal(line, &entry) == nil && entry["msg"] == "request" {
			access = entry
		}
	}
	if access == nil {
		t.Fatalf("no access log line for the panicking request: %s", logs.String())
	}
	if access["status"] != float64(http.StatusInternalServerError) {
		t.Errorf("logged status = %v, want 500", access["status"])
	}
}
