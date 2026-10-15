package httpapi

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/ronybrand/go-order-api/internal/apperr"
)

const (
	limiterIdleTTL       = 2 * time.Minute
	limiterPurgeInterval = time.Minute
)

type limiterEntry struct {
	limiter *rate.Limiter
	last    time.Time
}

// keyedLimiter is one token bucket per key (user, client address): perMinute requests per minute, with a
// burst of the same size. Idle keys are dropped so the map cannot grow forever.
type keyedLimiter struct {
	perMinute int
	now       func() time.Time

	mu        sync.Mutex
	entries   map[string]*limiterEntry
	lastPurge time.Time
}

func newKeyedLimiter(perMinute int, now func() time.Time) *keyedLimiter {
	return &keyedLimiter{perMinute: perMinute, now: now, entries: make(map[string]*limiterEntry), lastPurge: now()}
}

func (l *keyedLimiter) allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastPurge) > limiterPurgeInterval {
		for k, e := range l.entries {
			if now.Sub(e.last) > limiterIdleTTL {
				delete(l.entries, k)
			}
		}
		l.lastPurge = now
	}
	entry, found := l.entries[key]
	if !found {
		entry = &limiterEntry{limiter: rate.NewLimiter(rate.Limit(float64(l.perMinute)/60), l.perMinute)}
		l.entries[key] = entry
	}
	entry.last = now
	return entry.limiter.AllowN(now, 1)
}

// retryAfterSeconds is how long until one more token is available.
func (l *keyedLimiter) retryAfterSeconds() string {
	return strconv.Itoa((60 + l.perMinute - 1) / l.perMinute)
}

// rateLimit rejects with RATE-00 (429) the requests of a key that exhausted its budget.
func rateLimit(limiter *keyedLimiter, keyOf func(*http.Request) string, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.allow(keyOf(r)) {
				w.Header().Set("Retry-After", limiter.retryAfterSeconds())
				writeError(w, r, logger, apperr.RateLimited())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
