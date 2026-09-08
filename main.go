// Conductor — Construct's control plane for automations.
//
// Owns the automation rules + the schedule + claim/lease. Executors (the
// desktop brain, or the cloud fallback) claim due rules, run them, and report.
// Conductor never runs the agent or touches app data — it only coordinates.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var store *Store

func main() {
	dataDir := os.Getenv("CONDUCTOR_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	_ = os.MkdirAll(dataDir, 0o755)
	store = NewStore(filepath.Join(dataDir, "automations.json"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "service": "conductor"})
	})
	mux.HandleFunc("GET /api/automations", handleList)
	mux.HandleFunc("POST /api/automations", handleSave)
	mux.HandleFunc("DELETE /api/automations/{id}", handleDelete)
	mux.HandleFunc("POST /api/automations/{id}/run", handleRunNow)
	mux.HandleFunc("POST /api/presence", handlePresence)
	mux.HandleFunc("POST /api/claim", handleClaim)
	mux.HandleFunc("POST /api/report", handleReport)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}
	log.Printf("[conductor] listening on :%s (data: %s)", port, dataDir)
	log.Fatal(http.ListenAndServe(":"+port, withCORS(mux)))
}

// withCORS lets the desktop webview and my.lisaos.dev call Conductor
// directly (it sits outside the /api gateway, like graph.lisaos.dev).
// Reflects the request origin and answers preflight.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Internal-Secret")
			h.Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleRunNow forces a rule due so the next polling executor picks it up.
func handleRunNow(w http.ResponseWriter, r *http.Request) {
	uid, ok := resolveUser(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthenticated"})
		return
	}
	store.MarkDue(uid, r.PathValue("id"))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ── presence (observability + future routing) ─────────────────────────────
type presenceState struct {
	mu sync.Mutex
	m  map[string]map[string]int64 // userID -> executorID -> lastSeenUnix
}

var presence = &presenceState{m: map[string]map[string]int64{}}

func (p *presenceState) seen(userID, executorID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m[userID] == nil {
		p.m[userID] = map[string]int64{}
	}
	p.m[userID][executorID] = time.Now().Unix()
}

// active reports whether executorID was seen within the grace window. The
// desktop polls /api/claim every ~20s, so each poll refreshes its presence;
// once the desktop process stops, it drops out of "active" after the window
// and the cloud fallback may claim.
func (p *presenceState) active(userID, executorID string, within time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.m[userID]
	if m == nil {
		return false
	}
	ts, ok := m[executorID]
	if !ok {
		return false
	}
	return time.Since(time.Unix(ts, 0)) < within
}

// desktopGrace is how long after the desktop's last poll the cloud fallback
// keeps yielding to it. Comfortably longer than the desktop's ~20s poll.
const desktopGrace = 90 * time.Second

// ── handlers ──────────────────────────────────────────────────────────────
func handleList(w http.ResponseWriter, r *http.Request) {
	uid, ok := resolveUser(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthenticated"})
		return
	}
	writeJSON(w, 200, map[string]any{"automations": store.List(uid)})
}

func handleSave(w http.ResponseWriter, r *http.Request) {
	uid, ok := resolveUser(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthenticated"})
		return
	}
	var a Automation
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	a.UserID = uid // never trust a client-supplied owner
	writeJSON(w, 200, store.Save(&a))
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	uid, ok := resolveUser(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthenticated"})
		return
	}
	store.Delete(uid, r.PathValue("id"))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handlePresence(w http.ResponseWriter, r *http.Request) {
	uid, ok := resolveUser(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthenticated"})
		return
	}
	var b struct {
		ExecutorID string `json:"executor_id"`
		Kind       string `json:"kind"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	presence.seen(uid, b.ExecutorID)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleClaim(w http.ResponseWriter, r *http.Request) {
	// Service mode: the cloud executor authenticates with the internal secret
	// (no user identity) and claims across all users, getting a delegated token
	// back to run as the rule's owner.
	if trustedInternal(r) {
		handleServiceClaim(w, r)
		return
	}
	uid, ok := resolveUser(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthenticated"})
		return
	}
	var b struct {
		ExecutorID string `json:"executor_id"`
		LeaseSecs  int64  `json:"lease_secs"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	if b.LeaseSecs <= 0 {
		b.LeaseSecs = 180
	}
	presence.seen(uid, b.ExecutorID)
	// Cloud yields while an online desktop is still polling.
	deferToDesktop := presence.active(uid, desktopExecutor, desktopGrace)
	a := store.Claim(uid, b.ExecutorID, b.LeaseSecs, deferToDesktop)
	writeJSON(w, 200, map[string]any{"automation": a}) // null when nothing due
}

// handleServiceClaim is the cloud-executor path: claim the most-overdue rule
// across all users whose desktop isn't online, then mint a delegated token so
// the executor can run it as that user. If minting fails the lease is released
// so the rule isn't stranded until lease expiry.
func handleServiceClaim(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ExecutorID string `json:"executor_id"`
		LeaseSecs  int64  `json:"lease_secs"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	if b.LeaseSecs <= 0 {
		b.LeaseSecs = 180
	}
	if b.ExecutorID == "" {
		b.ExecutorID = "cloud"
	}
	a := store.ClaimAny(b.ExecutorID, b.LeaseSecs, func(uid string) bool {
		return presence.active(uid, desktopExecutor, desktopGrace)
	})
	if a == nil {
		writeJSON(w, 200, map[string]any{"automation": nil})
		return
	}
	token, err := mintDelegatedToken(a.UserID)
	if err != nil {
		store.Release(a.ID, b.ExecutorID)
		writeJSON(w, 502, map[string]any{"error": "delegated-token mint failed: " + err.Error()})
		return
	}
	// The executor uses this token for the run AND to report back (it resolves
	// to the rule's owner via accounts /me).
	writeJSON(w, 200, map[string]any{"automation": a, "token": token})
}

func handleReport(w http.ResponseWriter, r *http.Request) {
	uid, ok := resolveUser(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthenticated"})
		return
	}
	var b struct {
		ID         string `json:"id"`
		ExecutorID string `json:"executor_id"`
		Result     string `json:"result"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	store.Report(uid, b.ID, b.ExecutorID, b.Result)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
