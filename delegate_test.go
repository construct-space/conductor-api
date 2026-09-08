package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestClaimAnyCrossUserAndDesktopPreference(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "a.json"))
	s.Save(&Automation{UserID: "u1", Instruction: "x", IntervalMin: 5, Enabled: true})
	s.Save(&Automation{UserID: "u2", Instruction: "y", IntervalMin: 5, Enabled: true})

	// u1's desktop is online -> only u2's rule is claimable by the cloud.
	active := func(uid string) bool { return uid == "u1" }
	c := s.ClaimAny("cloud", 180, active)
	if c == nil || c.UserID != "u2" {
		t.Fatalf("expected to claim u2's rule (u1 desktop active), got %v", c)
	}
	// u2 now leased by "cloud"; a DIFFERENT executor finds nothing (u1 deferred
	// to its desktop, u2 still leased). Same-id re-claim would renew the lease.
	if c2 := s.ClaimAny("cloud-2", 180, active); c2 != nil {
		t.Fatalf("expected nothing claimable, got %v", c2)
	}
	// Cloud finishes u2 and reports it (so it's no longer due). Now with all
	// desktops quiet, the next claim picks up u1's rule.
	s.Report("u2", c.ID, "cloud", "ok")
	if c3 := s.ClaimAny("cloud", 180, func(string) bool { return false }); c3 == nil || c3.UserID != "u1" {
		t.Fatalf("expected u1's rule once desktop quiet, got %v", c3)
	}
}

func TestReleaseUndoesLease(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "a.json"))
	a := s.Save(&Automation{UserID: "u1", Instruction: "x", IntervalMin: 5, Enabled: true})
	if c := s.ClaimAny("cloud", 180, nil); c == nil {
		t.Fatal("should claim")
	}
	s.Release(a.ID, "cloud")
	// Released -> immediately claimable again (not waiting for lease expiry).
	if c := s.ClaimAny("cloud", 180, nil); c == nil {
		t.Fatal("expected re-claim after release")
	}
}

func TestMintDelegatedToken(t *testing.T) {
	var gotSecret, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecret = r.Header.Get("X-Internal-Secret")
		var b struct{ UserID string `json:"user_id"` }
		_ = jsonDecode(r, &b)
		gotUser = b.UserID
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"cat_test","expires_at":"x","user_id":"u1"}`))
	}))
	defer srv.Close()
	t.Setenv("ACCOUNTS_INTERNAL_URL", srv.URL)
	t.Setenv("INTERNAL_SHARED_SECRET", "shh")

	tok, err := mintDelegatedToken("u1")
	if err != nil {
		t.Fatalf("mint failed: %v", err)
	}
	if tok != "cat_test" {
		t.Fatalf("token = %q", tok)
	}
	if gotSecret != "shh" || gotUser != "u1" {
		t.Fatalf("accounts saw secret=%q user=%q", gotSecret, gotUser)
	}
}
