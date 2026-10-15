package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/auth"
)

var discard = slog.New(slog.DiscardHandler)

func ok() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %q", rec.Body.String())
	}
	return body
}

func TestRequestID_KeepsAValidUUIDAndReplacesAnythingElse(t *testing.T) {
	valid := uuid.NewString()
	var seen string
	h := requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = requestIDFrom(r.Context()) }))

	tests := map[string]struct {
		header   string
		wantSame bool
	}{
		"valid uuid":          {valid, true},
		"log injection":       {"abc\r\nX-Evil: 1", false},
		"not a uuid":          {"hello", false},
		"uuid without dashes": {strings.ReplaceAll(valid, "-", ""), false},
		"absent":              {"", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.header != "" {
				req.Header["X-Request-Id"] = []string{tt.header}
			}

			rec := do(h, req)

			if _, err := uuid.Parse(rec.Header().Get("X-Request-Id")); err != nil {
				t.Errorf("response id %q is not a uuid", rec.Header().Get("X-Request-Id"))
			}
			if (rec.Header().Get("X-Request-Id") == tt.header) != tt.wantSame || seen != rec.Header().Get("X-Request-Id") {
				t.Errorf("header=%q context=%q", rec.Header().Get("X-Request-Id"), seen)
			}
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	rec := do(securityHeaders(ok()), httptest.NewRequest(http.MethodGet, "/", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestBodyLimit(t *testing.T) {
	reads := func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			writeError(w, r, discard, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	h := bodyLimit(10)(http.HandlerFunc(reads))

	declared := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 11)))
	// A chunked body has no Content-Length: the limit must count the bytes actually read.
	chunked := httptest.NewRequest(http.MethodPost, "/", iotest.OneByteReader(strings.NewReader(strings.Repeat("x", 11))))
	chunked.ContentLength = -1
	within := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("1234567890"))

	if rec := do(h, declared); rec.Code != http.StatusRequestEntityTooLarge || errorBody(t, rec)["code"] != apperr.CodeBodyTooLarge {
		t.Errorf("declared: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, chunked); rec.Code != http.StatusRequestEntityTooLarge || errorBody(t, rec)["code"] != apperr.CodeBodyTooLarge {
		t.Errorf("chunked: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, within); rec.Code != http.StatusNoContent {
		t.Errorf("within: %d", rec.Code)
	}
}

func TestRecover_ReportsInternalErrorWithoutLeakingThePanic(t *testing.T) {
	h := recoverer(discard)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("secret detail") }))

	rec := do(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError || errorBody(t, rec)["code"] != apperr.CodeInternal || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestWriteError_MapsKindsToStatusAndHidesUnexpectedErrors(t *testing.T) {
	tests := map[string]struct {
		err    error
		status int
		code   string
	}{
		"validation":      {apperr.InvalidField("name", "bad"), 400, "VALIDATION-01"},
		"not found":       {apperr.NotFound(apperr.CodeOrderNotFound, "x", nil), 404, "RESOURCE-NOT-FOUND-02"},
		"conflict":        {apperr.Conflict(apperr.CodeDuplicateTaxID, "x", nil), 409, "CONFLICT-01"},
		"unauthenticated": {apperr.Unauthenticated(), 401, "AUTH-01"},
		"forbidden":       {apperr.Forbidden(), 403, "AUTH-02"},
		"too large":       {apperr.TooLarge(10), 413, "VALIDATION-12"},
		"rate limited":    {apperr.RateLimited(), 429, "RATE-00"},
		"unexpected":      {io.ErrUnexpectedEOF, 500, "INTERNAL-00"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()

			requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeError(w, r, discard, tt.err) })).ServeHTTP(rec, req)

			body := errorBody(t, rec)
			if rec.Code != tt.status || body["code"] != tt.code || body["request_id"] == "" || body["params"] == nil {
				t.Errorf("%d %v", rec.Code, body)
			}
			if name == "unexpected" && strings.Contains(rec.Body.String(), "EOF") {
				t.Error("internal error text leaked to the client")
			}
		})
	}
}

func TestCORS(t *testing.T) {
	h := cors([]string{"https://app.example"})(ok())

	allowed := httptest.NewRequest(http.MethodGet, "/", nil)
	allowed.Header.Set("Origin", "https://app.example")
	denied := httptest.NewRequest(http.MethodGet, "/", nil)
	denied.Header.Set("Origin", "https://evil.example")
	preflight := httptest.NewRequest(http.MethodOptions, "/orders", nil)
	preflight.Header.Set("Origin", "https://app.example")
	preflight.Header.Set("Access-Control-Request-Method", "POST")

	if got := do(h, allowed).Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("allowed origin header = %q", got)
	}
	if got := do(h, denied).Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("denied origin got header %q", got)
	}
	pre := do(h, preflight)
	if pre.Code != http.StatusNoContent || pre.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Errorf("preflight: %d %v", pre.Code, pre.Header())
	}
}

type fakeVerifier map[string]auth.Claims

func (f fakeVerifier) Verify(token string) (auth.Claims, error) {
	if c, found := f[token]; found {
		return c, nil
	}
	return auth.Claims{}, auth.ErrInvalidToken
}

func TestAuthenticate_AndRequireRole(t *testing.T) {
	verifier := fakeVerifier{"user-token": {Subject: "u1", Roles: []string{"user"}}, "admin-token": {Subject: "a1", Roles: []string{"admin"}}}
	var actor app.Actor
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor = app.ActorFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	h := authenticate(verifier, discard)(requireRole(auth.RoleAdmin, discard)(inner))
	call := func(authorization string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		return do(h, req)
	}

	for name, header := range map[string]string{"no header": "", "wrong scheme": "Basic abc", "bad token": "Bearer nope", "empty bearer": "Bearer "} {
		if rec := call(header); rec.Code != 401 || errorBody(t, rec)["code"] != "AUTH-01" || rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := call("Bearer user-token"); rec.Code != 403 || errorBody(t, rec)["code"] != "AUTH-02" {
		t.Errorf("user on admin route: %d %s", rec.Code, rec.Body)
	}
	if rec := call("Bearer admin-token"); rec.Code != 204 || actor.ID != "a1" {
		t.Errorf("admin: %d actor=%+v", rec.Code, actor)
	}
}

func TestRateLimit_PerKeyWithRetryAfter(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := newKeyedLimiter(2, func() time.Time { return clock })
	h := rateLimit(limiter, func(r *http.Request) string { return r.Header.Get("X-Key") }, discard)(ok())
	call := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Key", key)
		return do(h, req)
	}

	first, second, third := call("a"), call("a"), call("a")
	other := call("b")
	clock = clock.Add(time.Minute)
	afterRefill := call("a")

	if first.Code != 204 || second.Code != 204 {
		t.Errorf("within budget: %d %d", first.Code, second.Code)
	}
	if third.Code != 429 || errorBody(t, third)["code"] != "RATE-00" || third.Header().Get("Retry-After") == "" {
		t.Errorf("over budget: %d %s", third.Code, third.Body)
	}
	if other.Code != 204 || afterRefill.Code != 204 {
		t.Errorf("other key / refill: %d %d", other.Code, afterRefill.Code)
	}
}

func TestDecodeJSON_RejectsUnknownFieldsTrailingDataAndWrongTypes(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	tests := map[string]string{
		"unknown field": `{"name":"a","extra":1}`,
		"trailing data": `{"name":"a"} {"name":"b"}`,
		"wrong type":    `{"name":1}`,
		"empty":         ``,
		"not json":      `nope`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
			var p payload

			err := decodeJSON(req, &p)

			if !apperr.Is(err, apperr.CodeInvalidRequest) {
				t.Errorf("err = %v", err)
			}
		})
	}
	var p payload
	if err := decodeJSON(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ok"}`)), &p); err != nil || p.Name != "ok" {
		t.Errorf("valid body: %v %+v", err, p)
	}
}
