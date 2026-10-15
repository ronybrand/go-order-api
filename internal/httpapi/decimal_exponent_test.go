package httpapi

import (
	"net/http"
	"testing"
	"time"
)

func TestCreateOrder_ExtremeExponentsAreRejectedQuickly(t *testing.T) {
	for _, price := range []string{"1e999999999", "1e100000", "1e1000000", "1e-1000000", "0.0000e100000", "1e2147483647"} {
		t.Run(price, func(t *testing.T) {
			h := newHarness(t)
			body := `{"customer_id":"00000000-0000-0000-0000-0000000000cc","items":[{"description":"x","unit_price":` + price + `,"quantity":1}]}`
			start := time.Now()
			rec := h.call(http.MethodPost, "/orders", "user", body)
			t.Logf("%s -> %d in %s", price, rec.Code, time.Since(start))
			if rec.Code != http.StatusBadRequest || time.Since(start) > time.Second {
				t.Errorf("status=%d took=%s body=%s", rec.Code, time.Since(start), rec.Body)
			}
		})
	}
}
