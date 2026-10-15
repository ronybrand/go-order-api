package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/auth"
)

// Middleware is the shape of every wrapper in this package.
type Middleware = func(http.Handler) http.Handler

// chain applies middleware so that the first one listed is the outermost.
func chain(h http.Handler, middleware ...Middleware) http.Handler {
	for _, m := range slices.Backward(middleware) {
		h = m(h)
	}
	return h
}

// requestID keeps an incoming X-Request-Id only when it is a canonical UUID: anything else (log injection,
// absurdly long values) is replaced by a fresh one.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if parsed, err := uuid.Parse(id); err != nil || parsed.String() != strings.ToLower(id) {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		next.ServeHTTP(w, r)
	})
}

// bodyLimit refuses bodies above max bytes. A declared Content-Length is checked up front; for chunked
// bodies (no Content-Length) MaxBytesReader counts what is actually read, so the limit cannot be skipped.
func bodyLimit(max int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > max {
				writeError(w, r, nil, apperr.TooLarge(max))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}

func recoverer(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					if recovered == http.ErrAbortHandler { //nolint:errorlint // the sentinel is compared by identity on purpose
						panic(recovered)
					}
					logger.ErrorContext(r.Context(), "panic in handler", "panic", recovered, "request_id", requestIDFrom(r.Context()))
					writeError(w, r, logger, fmt.Errorf("panic in handler: %v", recovered)) // not an apperr: renders as INTERNAL-00
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// accessLog records one line per request. The query string is left out on purpose: search filters can carry
// PII (an e-mail, a tax id).
func accessLog(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.InfoContext(r.Context(), "request",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(), "request_id", requestIDFrom(r.Context()))
		})
	}
}

// cors allows exactly the configured origins; any other origin simply gets no CORS headers.
func cors(origins []string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" || !slices.Contains(origins, origin) {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
				h.Set("Access-Control-Max-Age", strconv.Itoa(600))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// TokenVerifier is what authentication needs from the auth package (defined here, where it is consumed).
type TokenVerifier interface {
	Verify(token string) (auth.Claims, error)
}

// authenticate requires a valid bearer token and puts the caller in the context as an app.Actor.
func authenticate(verifier TokenVerifier, logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fields := strings.Fields(r.Header.Get("Authorization"))
			if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, r, logger, apperr.Unauthenticated())
				return
			}
			claims, err := verifier.Verify(fields[1])
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				writeError(w, r, logger, apperr.Unauthenticated())
				return
			}
			actor := app.Actor{ID: claims.Subject, Roles: claims.Roles}
			next.ServeHTTP(w, r.WithContext(app.WithActor(r.Context(), actor)))
		})
	}
}

// requireRole must run after authenticate: with no actor in the context it denies.
func requireRole(role string, logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !slices.Contains(app.ActorFrom(r.Context()).Roles, role) {
				writeError(w, r, logger, apperr.Forbidden())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
