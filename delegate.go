// Service-mode auth + delegated-token minting.
//
// The desktop executor claims with the user's own bearer token. The CLOUD
// executor has no user credential — it authenticates to Conductor with the
// shared internal secret (X-Internal-Secret), claims across all users via
// ClaimAny, and Conductor mints a short-lived "act as user X" token from
// accounts so the cloud run can reach Graph/source as that user. The token is
// minted per-claim and expires in minutes, so a cloud box never holds a
// long-lived user credential.
package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const headerInternalSecret = "X-Internal-Secret"

// trustedInternal reports whether r carries the matching internal secret
// (constant-time, mirrors go-auth's Trusted). An unset secret always fails, so
// service-mode is dormant until INTERNAL_SHARED_SECRET is configured.
func trustedInternal(r *http.Request) bool {
	secret := os.Getenv("INTERNAL_SHARED_SECRET")
	if secret == "" {
		return false
	}
	got := r.Header.Get(headerInternalSecret)
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// accountsInternalURL is where the delegated-token endpoint lives. It is an
// internal, secret-gated endpoint (NOT under the public /api gateway), so it
// needs the service's internal address — set ACCOUNTS_INTERNAL_URL on deploy
// (e.g. http://srv-captain--accounts). Falls back to ACCOUNTS_URL.
func accountsInternalURL() string {
	if u := os.Getenv("ACCOUNTS_INTERNAL_URL"); u != "" {
		return u
	}
	return os.Getenv("ACCOUNTS_URL")
}

// mintDelegatedToken asks accounts for a short-lived token to act as userID.
func mintDelegatedToken(userID string) (string, error) {
	base := accountsInternalURL()
	if base == "" {
		return "", fmt.Errorf("ACCOUNTS_INTERNAL_URL not set")
	}
	secret := os.Getenv("INTERNAL_SHARED_SECRET")
	body, _ := json.Marshal(map[string]any{
		"user_id":  userID,
		"ttl_secs": 900,
		"scope":    "automations",
	})
	req, err := http.NewRequest("POST", strings.TrimRight(base, "/")+"/internal/delegated-token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set(headerInternalSecret, secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return "", fmt.Errorf("accounts mint failed: %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("accounts returned empty token")
	}
	return out.Token, nil
}
