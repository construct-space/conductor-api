package main

import (
	"path/filepath"
	"testing"
)

func TestClaimLeaseAndReport(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "a.json"))
	u := "user-1"

	// A never-run enabled rule is immediately due.
	a := s.Save(&Automation{UserID: u, Instruction: "do x", IntervalMin: 15, Enabled: true})

	// Desktop claims it.
	c1 := s.Claim(u, "desktop", 180, false)
	if c1 == nil || c1.ID != a.ID {
		t.Fatalf("expected to claim the due rule, got %v", c1)
	}
	// A second executor can't claim it while leased.
	if c2 := s.Claim(u, "cloud", 180, false); c2 != nil {
		t.Fatalf("expected no claim while leased, got %v", c2)
	}
	// After report, lastRunAt set → not due again until interval passes.
	s.Report(u, a.ID, "desktop", "did x")
	if c3 := s.Claim(u, "desktop", 180, false); c3 != nil {
		t.Fatalf("expected not-due after report, got %v", c3)
	}
	got := s.List(u)
	if len(got) != 1 || got[0].LastResult != "did x" || got[0].LeasedBy != "" {
		t.Fatalf("report state wrong: %+v", got)
	}
}

func TestClaimRespectsEnabledAndTenant(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "a.json"))
	s.Save(&Automation{UserID: "u1", Instruction: "x", IntervalMin: 5, Enabled: false}) // disabled
	s.Save(&Automation{UserID: "u2", Instruction: "y", IntervalMin: 5, Enabled: true})  // other user
	if c := s.Claim("u1", "e", 60, false); c != nil {
		t.Fatal("disabled rule should not be claimable")
	}
	if c := s.Claim("u1", "e", 60, false); c != nil {
		t.Fatal("u1 must not claim u2's rule")
	}
	if c := s.Claim("u2", "e", 60, false); c == nil {
		t.Fatal("u2 should claim its own enabled rule")
	}
}

func TestDeferToDesktop(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "a.json"))
	u := "user-1"
	a := s.Save(&Automation{UserID: u, Instruction: "do x", IntervalMin: 15, Enabled: true})

	// Cloud yields while the desktop is active.
	if c := s.Claim(u, "cloud", 180, true); c != nil {
		t.Fatal("cloud must defer to an active desktop")
	}
	// The desktop is never blocked by its own preference flag.
	if c := s.Claim(u, "desktop", 180, true); c == nil || c.ID != a.ID {
		t.Fatalf("desktop should always claim, got %v", c)
	}
	s.Report(u, a.ID, "desktop", "did x")

	// Once the desktop goes quiet (deferToDesktop=false), the cloud takes over.
	a2 := s.Save(&Automation{UserID: u, Instruction: "do y", IntervalMin: 15, Enabled: true})
	if c := s.Claim(u, "cloud", 180, false); c == nil || c.ID != a2.ID {
		t.Fatalf("cloud should claim when desktop is offline, got %v", c)
	}
}
