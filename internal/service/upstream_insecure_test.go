package service

import (
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// SC-02 (levis side): upstream interface configs must not send credentials
// over plain HTTP to non-loopback hosts unless the operator explicitly sets
// allow_insecure=true on that interface.
func TestInterfaceRejectsPlainHTTPNonLoopbackByDefault(t *testing.T) {
	db := newTestDB(t)
	s := NewUpstreamService(db, nil)
	bad := InterfaceInput{
		Name: "prod-http", PluginID: "virtualis",
		Config: map[string]string{"api_url": "http://upstream.example.com", "api_key": "k"},
	}
	if _, err := s.Create(bad); err == nil {
		t.Fatal("plain-HTTP non-loopback upstream must be rejected without allow_insecure")
	}
}

func TestInterfaceRejectsBadAllowInsecureValue(t *testing.T) {
	db := newTestDB(t)
	s := NewUpstreamService(db, nil)
	bad := InterfaceInput{
		Name: "prod-http-2", PluginID: "virtualis",
		Config: map[string]string{"api_url": "http://upstream.example.com", "allow_insecure": "yes"},
	}
	if _, err := s.Create(bad); err == nil {
		t.Fatal("allow_insecure must be an explicit true/false, not a fuzzy truthy string")
	}
}

func TestInterfaceAllowsPlainHTTPWithExplicitOptIn(t *testing.T) {
	db := newTestDB(t)
	s := NewUpstreamService(db, nil)
	ok := InterfaceInput{
		Name: "legacy-http", PluginID: "virtualis",
		Config: map[string]string{"api_url": "http://upstream.example.com", "allow_insecure": "true"},
	}
	item, err := s.Create(ok)
	if err != nil {
		t.Fatalf("explicit opt-in must allow plain HTTP: %v", err)
	}
	if item.Config["allow_insecure"] != "true" {
		t.Fatalf("opt-in flag must persist, got %q", item.Config["allow_insecure"])
	}
}

func TestInterfaceAllowsLoopbackAndHTTPSWithoutOptIn(t *testing.T) {
	db := newTestDB(t)
	s := NewUpstreamService(db, nil)
	for _, tc := range []struct{ name, url string }{
		{"loopback-127", "http://127.0.0.1:8080"},
		{"loopback-localhost", "http://localhost:8080"},
		{"https", "https://upstream.example.com"},
		{"no-url", ""},
	} {
		in := InterfaceInput{Name: tc.name, PluginID: "virtualis", Config: map[string]string{"api_url": tc.url}}
		if _, err := s.Create(in); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}

// Legacy rows (no allow_insecure key) that already carry non-loopback HTTP
// must keep working after upgrade — the migration default is "allow" — but
// new edits re-run validation, so flipping anything re-derives the flag.
func TestInterfaceUpdatePreservesLegacyPlainHTTPRow(t *testing.T) {
	db := newTestDB(t)
	// Simulate a pre-upgrade row: plain HTTP, no allow_insecure flag.
	legacy := model.UpstreamInterface{
		Name: "legacy", PluginID: "virtualis",
		Config: model.OptionMap{"api_url": "http://10.0.0.8:8080", "api_key": "k"},
	}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	s := NewUpstreamService(db, nil)
	got, err := s.Interface(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The migration default must materialize allow_insecure=true so the
	// existing deployment keeps working (an implicit break would strand
	// production provisioning).
	if got.Config["allow_insecure"] != "true" {
		t.Fatalf("legacy plain-HTTP row must be read back with allow_insecure=true, got %q", got.Config["allow_insecure"])
	}
}
