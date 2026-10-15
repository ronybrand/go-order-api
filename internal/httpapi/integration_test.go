package httpapi_test

// End-to-end tests: real HTTP stack, real services, real PostgreSQL (TEST_DATABASE_URL), real RS256 tokens.
// Skipped when the variable is not set.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/auth"
	"github.com/ronybrand/go-order-api/internal/domain"
	"github.com/ronybrand/go-order-api/internal/httpapi"
	"github.com/ronybrand/go-order-api/internal/postgres"
)

const (
	audience = "order-api"
	issuer   = "https://idp.example"
)

type recordingPublisher struct {
	mu     sync.Mutex
	events []domain.OrderStatusChanged
}

func (p *recordingPublisher) PublishStatusChanged(_ context.Context, e domain.OrderStatusChanged) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	return nil
}

type env struct {
	server    *httptest.Server
	key       *rsa.PrivateKey
	publisher *recordingPublisher
}

func newEnv(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool, logger); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE order_items, orders, customers CASCADE"); err != nil {
		t.Fatal(err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	verifier, err := auth.NewVerifier(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), audience, issuer)
	if err != nil {
		t.Fatal(err)
	}

	store := postgres.NewStore(pool)
	publisher := &recordingPublisher{}
	handler := httpapi.New(httpapi.Options{
		Customers: app.NewCustomerService(store, app.WithLogger(logger)),
		Orders:    app.NewOrderService(store, publisher, app.WithLogger(logger)),
		Verifier:  verifier, Logger: logger, Ping: pool.Ping,
		MaxBodyBytes: 4096, RateLimitPerUser: 10_000, RateLimitWrites: 10_000, RateLimitPerIP: 10_000,
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &env{server: server, key: key, publisher: publisher}
}

func (e *env) token(t *testing.T, subject string, roles ...string) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": subject, "aud": audience, "iss": issuer, "roles": roles, "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(e.key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

type response struct {
	status int
	body   map[string]any
}

func (e *env) do(t *testing.T, method, path, token, body string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := response{status: res.StatusCode}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (r response) code() string { c, _ := r.body["code"].(string); return c }

func (e *env) createCustomer(t *testing.T, admin, taxID, email string) string {
	t.Helper()
	res := e.do(t, "POST", "/customers", admin,
		`{"name":"Ana","tax_id":"`+taxID+`","email":"`+email+`"}`)
	if res.status != 201 {
		t.Fatalf("create customer: %d %v", res.status, res.body)
	}
	return res.body["id"].(string)
}

func TestFullOrderLifecycle(t *testing.T) {
	e := newEnv(t)
	admin, user := e.token(t, "admin-1", auth.RoleAdmin), e.token(t, "user-1", auth.RoleUser)
	customerID := e.createCustomer(t, admin, "123.456.789-00", "ana@example.com")

	created := e.do(t, "POST", "/orders", user, `{"customer_id":"`+customerID+`","items":[{"description":"Pen","unit_price":2.50,"quantity":4}]}`)
	orderID, _ := created.body["id"].(string)
	added := e.do(t, "POST", "/orders/"+orderID+"/items", user, `{"description":"Ink","unit_price":0.10,"quantity":3}`)
	confirmed := e.do(t, "POST", "/orders/"+orderID+"/confirm", user, "")
	editAfter := e.do(t, "POST", "/orders/"+orderID+"/items", user, `{"description":"Late","unit_price":1,"quantity":1}`)
	canceled := e.do(t, "POST", "/orders/"+orderID+"/cancel", user, "")
	cancelAgain := e.do(t, "POST", "/orders/"+orderID+"/cancel", user, "")

	if created.status != 201 || created.body["status"] != "OPEN" || created.body["total"].(float64) != 10 {
		t.Fatalf("create: %d %v", created.status, created.body)
	}
	if added.status != 200 || added.body["total"].(float64) != 10.3 || len(added.body["items"].([]any)) != 2 {
		t.Errorf("add item: %d %v", added.status, added.body)
	}
	if confirmed.status != 200 || confirmed.body["status"] != "CONFIRMED" {
		t.Errorf("confirm: %d %v", confirmed.status, confirmed.body)
	}
	if editAfter.status != 400 || editAfter.code() != "VALIDATION-02" {
		t.Errorf("edit after confirm: %d %v", editAfter.status, editAfter.body)
	}
	if canceled.status != 200 || canceled.body["status"] != "CANCELED" {
		t.Errorf("cancel: %d %v", canceled.status, canceled.body)
	}
	if cancelAgain.status != 400 || cancelAgain.code() != "VALIDATION-08" {
		t.Errorf("cancel again: %d %v", cancelAgain.status, cancelAgain.body)
	}
	if got := len(e.publisher.events); got != 2 {
		t.Fatalf("published %d events, want 2 (confirm and cancel)", got)
	}
	if ev := e.publisher.events[0]; ev.CustomerEmail != "ana@example.com" || ev.OldStatus != "OPEN" || ev.NewStatus != "CONFIRMED" {
		t.Errorf("first event = %+v", ev)
	}
}

func TestCustomerRulesOverHTTP(t *testing.T) {
	e := newEnv(t)
	admin, user := e.token(t, "admin-1", auth.RoleAdmin), e.token(t, "user-1", auth.RoleUser)
	customerID := e.createCustomer(t, admin, "12345678", "ana@example.com")

	duplicate := e.do(t, "POST", "/customers", admin, `{"name":"B","tax_id":"123-456.78","email":"b@example.com"}`)
	asUser := e.do(t, "POST", "/customers", user, `{"name":"C","tax_id":"99999999","email":"c@example.com"}`)
	invalid := e.do(t, "POST", "/customers", admin, `{"name":"D","tax_id":"x","email":"nope"}`)
	order := e.do(t, "POST", "/orders", user, `{"customer_id":"`+customerID+`"}`)
	blocked := e.do(t, "DELETE", "/customers/"+customerID, admin, "")
	e.do(t, "DELETE", "/orders/"+order.body["id"].(string), user, "")
	deleted := e.do(t, "DELETE", "/customers/"+customerID, admin, "")
	gone := e.do(t, "GET", "/customers/"+customerID, user, "")
	orderForGone := e.do(t, "POST", "/orders", user, `{"customer_id":"`+customerID+`"}`)

	if duplicate.status != 409 || duplicate.code() != "CONFLICT-01" {
		t.Errorf("duplicate tax id: %d %v", duplicate.status, duplicate.body)
	}
	if asUser.status != 403 || asUser.code() != "AUTH-02" {
		t.Errorf("user creating customer: %d %v", asUser.status, asUser.body)
	}
	if invalid.status != 400 || invalid.code() != "VALIDATION-01" {
		t.Errorf("invalid customer: %d %v", invalid.status, invalid.body)
	}
	if blocked.status != 409 || blocked.code() != "CONFLICT-03" {
		t.Errorf("delete with orders: %d %v", blocked.status, blocked.body)
	}
	if deleted.status != 204 || gone.status != 404 || gone.code() != "RESOURCE-NOT-FOUND-01" {
		t.Errorf("delete/get: %d %d %v", deleted.status, gone.status, gone.body)
	}
	if orderForGone.status != 400 || orderForGone.code() != "VALIDATION-07" {
		t.Errorf("order for deleted customer: %d %v", orderForGone.status, orderForGone.body)
	}
}

func TestSearchOverHTTP(t *testing.T) {
	e := newEnv(t)
	admin, user := e.token(t, "admin-1", auth.RoleAdmin), e.token(t, "user-1", auth.RoleUser)
	e.createCustomer(t, admin, "11111111", "a@example.com")
	e.createCustomer(t, admin, "22222222", "b@example.com")

	paged := e.do(t, "GET", "/customers/search?page=1&size=1&sort=-created_at", user, "")
	filtered := e.do(t, "POST", "/customers/search", user, `{"size":10,"filters":{"tax_id":[{"op":"eq","value":"22222222"}]}}`)
	badSort := e.do(t, "GET", "/customers/search?sort=password", user, "")
	badType := e.do(t, "POST", "/orders/search", user, `{"filters":{"version":[{"op":"eq","value":"abc"}]}}`)

	if paged.status != 200 || paged.body["total"].(float64) != 2 || len(paged.body["items"].([]any)) != 1 {
		t.Errorf("paged: %d %v", paged.status, paged.body)
	}
	if filtered.status != 200 || filtered.body["total"].(float64) != 1 {
		t.Errorf("filtered: %d %v", filtered.status, filtered.body)
	}
	if badSort.status != 400 || badSort.code() != "VALIDATION-06" {
		t.Errorf("unknown sort: %d %v", badSort.status, badSort.body)
	}
	if badType.status != 400 || badType.code() != "VALIDATION-05" {
		t.Errorf("wrong filter type: %d %v", badType.status, badType.body)
	}
}

func TestEdgeProtections(t *testing.T) {
	e := newEnv(t)
	admin := e.token(t, "admin-1", auth.RoleAdmin)

	noToken := e.do(t, "GET", "/orders/search", "", "")
	forged := e.do(t, "GET", "/orders/search", "not.a.token", "")
	tooBig := e.do(t, "POST", "/customers", admin, `{"name":"`+strings.Repeat("x", 5000)+`"}`)
	health := e.do(t, "GET", "/health", "", "")

	if noToken.status != 401 || forged.status != 401 || noToken.code() != "AUTH-01" {
		t.Errorf("auth: %d %d %v", noToken.status, forged.status, noToken.body)
	}
	if tooBig.status != 413 || tooBig.code() != "VALIDATION-12" {
		t.Errorf("oversize body: %d %v", tooBig.status, tooBig.body)
	}
	if health.status != 200 {
		t.Errorf("health: %d", health.status)
	}
}
