package agentfw

import (
	"fmt"
	"net/http"
	"sync/atomic"

	"golang.org/x/time/rate"
)

// RateLimiter enforces per-proxy request rate and total data budget.
type RateLimiter struct {
	limiter     *rate.Limiter // nil = unlimited
	budgetBytes int64         // 0 = unlimited
	usedBytes   atomic.Int64
}

func NewRateLimiter(requestsPerMinute int, dataBudgetMB int) *RateLimiter {
	rl := &RateLimiter{}
	if requestsPerMinute > 0 {
		// ponytail: burst = requestsPerMinute/10 (min 1) — smooth rate, not bursty
		burst := requestsPerMinute / 10
		if burst < 1 {
			burst = 1
		}
		rl.limiter = rate.NewLimiter(rate.Limit(float64(requestsPerMinute)/60.0), burst)
	}
	if dataBudgetMB > 0 {
		rl.budgetBytes = int64(dataBudgetMB) << 20
	}
	return rl
}

// Allow checks rate and budget. Returns an error if the request should be blocked.
func (rl *RateLimiter) Allow(bodyBytes int64) error {
	if rl.limiter != nil && !rl.limiter.Allow() {
		return fmt.Errorf("agentfw: rate limit exceeded")
	}
	if rl.budgetBytes > 0 {
		used := rl.usedBytes.Add(bodyBytes)
		if used > rl.budgetBytes {
			return fmt.Errorf("agentfw: data budget exceeded (%d MB)", rl.budgetBytes>>20)
		}
	}
	return nil
}

// Middleware wraps a handler with rate + budget enforcement.
func (rl *RateLimiter) Middleware(next http.Handler, auditor *Auditor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size := r.ContentLength
		if size < 0 {
			size = 0
		}
		if err := rl.Allow(size); err != nil {
			auditor.Log(Event{Method: r.Method, URL: r.URL.String(), Action: "block", Error: err.Error()})
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
