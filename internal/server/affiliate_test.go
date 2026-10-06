package server

import (
	"net/http"
	"testing"
)

func TestAffiliateIdentityAndRegisterAttribution(t *testing.T) {
	rt, h, admin, users := installedWithUsers(t, "referrer")
	if r := do(t, h, "GET", "/api/affiliate", nil); r.Code != 401 {
		t.Fatalf("unauthenticated affiliate=%d", r.Code)
	}
	settings := map[string]any{"enabled": true, "rate_bps": 777, "min_withdrawal_cents": 100}
	if r := doAs(t, h, "PUT", "/api/admin/settings/affiliate", settings, users["referrer"]); r.Code != 403 {
		t.Fatalf("user settings=%d", r.Code)
	}
	if r := doAs(t, h, "PUT", "/api/admin/settings/affiliate", settings, admin); r.Code != 200 {
		t.Fatalf("settings=%d %s", r.Code, r.Body.String())
	}
	r := doAs(t, h, "POST", "/api/affiliate/join", nil, users["referrer"])
	if r.Code != 200 {
		t.Fatalf("join=%d %s", r.Code, r.Body.String())
	}
	var a struct {
		Code          string `json:"code"`
		ReferralCount int64  `json:"referral_count"`
	}
	decodeJSON(t, r, &a)
	if len(a.Code) < 16 || a.ReferralCount != 0 {
		t.Fatalf("identity=%+v", a)
	}
	r = doAs(t, h, "POST", "/api/affiliate/join", nil, users["referrer"])
	var again struct {
		Code string `json:"code"`
	}
	decodeJSON(t, r, &again)
	if again.Code != a.Code {
		t.Fatal("join changed code")
	}
	r = do(t, h, http.MethodPost, "/api/auth/register", map[string]string{"username": "referred", "email": "referred@example.com", "password": "password123", "referral_code": a.Code})
	if r.Code != 200 {
		t.Fatalf("attributed register=%d %s", r.Code, r.Body.String())
	}
	var count int64
	if err := rt.DB().Table("affiliate_referrals").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("attribution missing: %d %v", count, err)
	}
	r = do(t, h, "POST", "/api/auth/register", map[string]string{"username": "badref", "email": "badref@example.com", "password": "password123", "referral_code": "not-a-valid-code"})
	if r.Code != 400 {
		t.Fatalf("invalid referral=%d", r.Code)
	}
	r = doAs(t, h, "GET", "/api/affiliate", nil, users["referrer"])
	decodeJSON(t, r, &a)
	if a.ReferralCount != 1 {
		t.Fatalf("count=%d", a.ReferralCount)
	}
}
