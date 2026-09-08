// Conductor store — per-user automation rules + claim/lease state.
//
// The control plane owns the rules and the schedule (the brain used to, but a
// clock in a process that can be off fires nothing). Executors (the desktop
// brain, or the cloud fallback) are stateless claimants: they claim a due
// automation, run it, and report the result. A lease prevents double-runs and
// lets the desktop be preferred (it claims first when online).
//
// Persistence is a single JSON file — no external DB — so the service is
// self-contained and trivially deployable. Fine at this scale; swap for SQLite
// if rule counts grow.
package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type Automation struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	Instruction string `json:"instruction"`
	IntervalMin int    `json:"interval_min"`
	Enabled     bool   `json:"enabled"`
	LastRunAt   int64  `json:"last_run_at,omitempty"`
	LastResult  string `json:"last_result,omitempty"`
	CreatedAt   int64  `json:"created_at,omitempty"`

	// Lease — set when an executor claims this rule for a run.
	LeasedBy   string `json:"leased_by,omitempty"`
	LeaseUntil int64  `json:"lease_until,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	path string
	byID map[string]*Automation
}

func NewStore(path string) *Store {
	s := &Store{path: path, byID: map[string]*Automation{}}
	s.load()
	return s
}

func (s *Store) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var list []*Automation
	if json.Unmarshal(b, &list) == nil {
		for _, a := range list {
			s.byID[a.ID] = a
		}
	}
}

func (s *Store) persist() {
	list := make([]*Automation, 0, len(s.byID))
	for _, a := range s.byID {
		list = append(list, a)
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	_ = os.WriteFile(s.path, b, 0o644)
}

func (s *Store) List(userID string) []*Automation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*Automation{}
	for _, a := range s.byID {
		if a.UserID == userID {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out
}

func (s *Store) Save(a *Automation) *Automation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.ID == "" {
		a.ID = newID()
		a.CreatedAt = nowUnix()
	}
	if cur, ok := s.byID[a.ID]; ok {
		// preserve run/lease state across edits
		a.LastRunAt, a.LastResult, a.CreatedAt = cur.LastRunAt, cur.LastResult, cur.CreatedAt
		a.LeasedBy, a.LeaseUntil = cur.LeasedBy, cur.LeaseUntil
	}
	s.byID[a.ID] = a
	s.persist()
	cp := *a
	return &cp
}

func (s *Store) Delete(userID, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.byID[id]; ok && a.UserID == userID {
		delete(s.byID, id)
		s.persist()
	}
}

// Claim atomically leases the next due automation for a user, preferring the
// most-overdue. Returns nil if nothing is due/claimable. leaseSecs is how long
// the executor has to run+report before another executor may take it.
//
// deferToDesktop implements the desktop-preference rule: when an active desktop
// has been seen recently, a NON-desktop executor (the cloud fallback) yields —
// the desktop gets first refusal. The handler computes this from presence so a
// fast-polling cloud box can't steal rules from an online desktop. Without it,
// the cloud only steps in once the desktop has gone quiet (presence grace).
func (s *Store) Claim(userID, executorID string, leaseSecs int64, deferToDesktop bool) *Automation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if deferToDesktop && executorID != desktopExecutor {
		return nil
	}
	now := nowUnix()
	var pick *Automation
	for _, a := range s.byID {
		if a.UserID != userID || !a.Enabled || a.IntervalMin <= 0 {
			continue
		}
		// not due yet?
		if a.LastRunAt != 0 && now-a.LastRunAt < int64(a.IntervalMin)*60 {
			continue
		}
		// currently leased by someone else (and not expired)?
		if a.LeaseUntil > now && a.LeasedBy != executorID {
			continue
		}
		if pick == nil || a.LastRunAt < pick.LastRunAt {
			pick = a
		}
	}
	if pick == nil {
		return nil
	}
	pick.LeasedBy = executorID
	pick.LeaseUntil = now + leaseSecs
	s.persist()
	cp := *pick
	return &cp
}

// ClaimAny leases the most-overdue due rule across ALL users for a non-user
// executor (the cloud fallback). It skips any rule whose owner has an active
// desktop — the desktop is preferred and claims its own. Used by the
// service-mode claim path, where the caller authenticates with the internal
// secret and has no single user identity. desktopActive(userID) reports
// whether that owner's desktop is currently polling.
func (s *Store) ClaimAny(executorID string, leaseSecs int64, desktopActive func(userID string) bool) *Automation {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowUnix()
	var pick *Automation
	for _, a := range s.byID {
		if !a.Enabled || a.IntervalMin <= 0 {
			continue
		}
		if a.LastRunAt != 0 && now-a.LastRunAt < int64(a.IntervalMin)*60 {
			continue
		}
		if a.LeaseUntil > now && a.LeasedBy != executorID {
			continue
		}
		if desktopActive != nil && desktopActive(a.UserID) {
			continue // owner's desktop will handle it
		}
		if pick == nil || a.LastRunAt < pick.LastRunAt {
			pick = a
		}
	}
	if pick == nil {
		return nil
	}
	pick.LeasedBy = executorID
	pick.LeaseUntil = now + leaseSecs
	s.persist()
	cp := *pick
	return &cp
}

// Release drops a lease without recording a run — used when an executor claims
// a rule but can't proceed (e.g. token mint failed), so another executor can
// retry it immediately instead of waiting for the lease to expire.
func (s *Store) Release(id, executorID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.byID[id]; ok && a.LeasedBy == executorID {
		a.LeasedBy = ""
		a.LeaseUntil = 0
		s.persist()
	}
}

// Report records a run result and clears the lease.
func (s *Store) Report(userID, id, executorID, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[id]
	if !ok || a.UserID != userID {
		return
	}
	a.LastRunAt = nowUnix()
	a.LastResult = result
	a.LeasedBy = ""
	a.LeaseUntil = 0
	s.persist()
}

// desktopExecutor is the reserved id the desktop brain claims under; it always
// wins over the cloud fallback (see Claim's deferToDesktop).
const desktopExecutor = "desktop"

// MarkDue makes a rule due right now (clears LastRunAt + any lease) so the next
// polling executor claims it. Backs the UI's "run now" — in a distributed model
// "run" means "schedule immediately," not "run in this process."
func (s *Store) MarkDue(userID, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.byID[id]; ok && a.UserID == userID {
		a.LastRunAt = 0
		a.LeasedBy = ""
		a.LeaseUntil = 0
		s.persist()
	}
}

func nowUnix() int64 { return time.Now().Unix() }

func newID() string { return "auto-" + time.Now().Format("20060102150405.000000000") }
