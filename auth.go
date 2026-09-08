package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// resolveUser maps a bearer token to a user id. In production it asks accounts
// (the gateway-fronted identity service). For local testing, CONDUCTOR_DEV_USER
// short-circuits to a fixed id so the loop is testable without a live accounts.
type userCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}
type cacheEntry struct {
	userID string
	at     time.Time
}

var ucache = &userCache{m: map[string]cacheEntry{}}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

func resolveUser(r *http.Request) (string, bool) {
	if dev := os.Getenv("CONDUCTOR_DEV_USER"); dev != "" {
		return dev, true
	}
	tok := bearer(r)
	if tok == "" {
		return "", false
	}
	ucache.mu.Lock()
	if e, ok := ucache.m[tok]; ok && time.Since(e.at) < 5*time.Minute {
		ucache.mu.Unlock()
		return e.userID, true
	}
	ucache.mu.Unlock()

	base := os.Getenv("ACCOUNTS_URL")
	if base == "" {
		base = "https://my.lisaos.dev/api/accounts"
	}
	req, _ := http.NewRequest("GET", strings.TrimRight(base, "/")+"/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return "", false
	}
	defer resp.Body.Close()
	var me struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&me)
	uid := me.User.ID
	if uid == "" {
		uid = me.ID
	}
	if uid == "" {
		return "", false
	}
	ucache.mu.Lock()
	ucache.m[tok] = cacheEntry{userID: uid, at: time.Now()}
	ucache.mu.Unlock()
	return uid, true
}
