package service

import (
	"context"
	"testing"
	"time"
)

// exact proration checks: money is integer cents and the calendar defines the
// ratio. Fixed clock, no tolerance bands (audit MONEY-08).
func TestChangeQuoteProrationExactCalendar(t *testing.T) {
	cases := []struct {
		name   string
		now    time.Time
		expiry time.Time
	}{
		// 30-day-ish months across a 3-month prepaid window.
		{"nov-to-feb", time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC)},
		// Long and short months mixed in a quarterly window.
		{"jan-to-apr", time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC), time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)},
		// Leap-year February inside an annual window tail.
		{"leap-window", time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC), time.Date(2028, 5, 29, 0, 0, 0, 0, time.UTC)},
		// Cross-year stack.
		{"dec-to-mar", time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC), time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newChangeFenceFixture(t, "prorate-"+tc.name, tc.expiry, 1000, 2000)
			q, err := fx.service.quote(fx.svc, fx.target, fx.fake.host, ChangeInput{ProductID: fx.target.ID}, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			wantRemaining := int64(tc.expiry.Sub(tc.now) / time.Second)
			if q.RemainingSeconds != wantRemaining {
				t.Fatalf("remaining = %d, want %d", q.RemainingSeconds, wantRemaining)
			}
			if q.TotalSeconds < wantRemaining {
				t.Fatalf("total %d must cover the whole prepaid window (remaining %d)", q.TotalSeconds, wantRemaining)
			}
			// Integer math: charge must equal floor(diff * remaining / cycleSeconds).
			diff := int64(1000)
			wantCharge, err := mulDivCents(diff, wantRemaining, int64(tc.expiry.Sub(tc.expiry.AddDate(0, -1, 0))/time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if q.ChargeCents != wantCharge {
				t.Fatalf("charge = %d, want exact floor %d", q.ChargeCents, wantCharge)
			}
		})
	}
}

// Downgrade credits use the same calendar basis, never the last-cycle division.
func TestChangeQuoteProrationExactCalendarDowngrade(t *testing.T) {
	now := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	expiry := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	fx := newChangeFenceFixture(t, "prorate-exact-down", expiry, 2000, 1000)
	q, err := fx.service.quote(fx.svc, fx.target, fx.fake.host, ChangeInput{ProductID: fx.target.ID}, now)
	if err != nil {
		t.Fatal(err)
	}
	wantRemaining := int64(expiry.Sub(now) / time.Second)
	wantCredit, err := mulDivCents(1000, wantRemaining, int64(expiry.Sub(expiry.AddDate(0, -1, 0))/time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if q.CreditCents != wantCredit {
		t.Fatalf("credit = %d, want exact floor %d", q.CreditCents, wantCredit)
	}
}

// MONEY-08 overflow: huge prepaid window and prices must not silently wrap.
func TestChangeQuoteProrationOverflow(t *testing.T) {
	fx := newChangeFenceFixture(t, "prorate-overflow", time.Date(2400, 1, 1, 0, 0, 0, 0, time.UTC), 1, 9223372036854775807)
	if _, err := fx.service.Preview(context.Background(), fx.user.ID, fx.svc.ID, ChangeInput{ProductID: fx.target.ID}); err == nil {
		t.Fatal("overflow-prone quote must be rejected, not wrapped")
	}
}
