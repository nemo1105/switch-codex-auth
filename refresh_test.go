package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRefreshAuthAliasesRefreshesGroupsSkipsFailuresAndLeavesActiveAuthUnchanged(t *testing.T) {
	dir := t.TempDir()

	fixedNow := time.Date(2026, 4, 3, 14, 30, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	sharedAlias := map[string]any{
		"tokens": map[string]any{
			"id_token":      "shared-old-id",
			"access_token":  "shared-old-access",
			"refresh_token": "shared-rt",
			"scope":         "keep-me",
		},
		"custom_field": map[string]any{
			"team": "alpha",
		},
	}
	sharedAlias2 := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      "shared2-old-id",
			"access_token":  "shared2-old-access",
			"refresh_token": "shared-rt",
			"account_id":    "acct-1",
		},
	}
	soloAlias := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      "solo-old-id",
			"access_token":  "solo-old-access",
			"refresh_token": "solo-rt",
		},
	}
	apiKeyAlias := map[string]any{
		"OPENAI_API_KEY": "sk-test",
	}
	noTokensAlias := map[string]any{
		"auth_mode": "chatgpt",
	}
	noRefreshAlias := map[string]any{
		"tokens": map[string]any{
			"access_token": "missing-refresh",
		},
	}

	activeBytes := writeJSONFile(t, filepath.Join(dir, "auth.json"), sharedAlias)
	writeJSONFile(t, filepath.Join(dir, "auth.json.a"), sharedAlias)
	writeJSONFile(t, filepath.Join(dir, "auth.json.b"), sharedAlias2)
	writeJSONFile(t, filepath.Join(dir, "auth.json.solo"), soloAlias)
	writeJSONFile(t, filepath.Join(dir, "auth.json.apikey"), apiKeyAlias)
	writeJSONFile(t, filepath.Join(dir, "auth.json.notokens"), noTokensAlias)
	writeJSONFile(t, filepath.Join(dir, "auth.json.norefresh"), noRefreshAlias)
	if err := os.WriteFile(filepath.Join(dir, "auth.json.bad"), []byte(`{"tokens":`), 0o600); err != nil {
		t.Fatalf("write malformed auth: %v", err)
	}

	var (
		mu     sync.Mutex
		counts = map[string]int{}
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}

		var req refreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		mu.Lock()
		counts[req.RefreshToken]++
		mu.Unlock()

		switch req.RefreshToken {
		case "shared-rt":
			writeJSONResponse(t, w, map[string]any{
				"id_token":      "shared-new-id",
				"access_token":  "shared-new-access",
				"refresh_token": "shared-new-rt",
			})
		case "solo-rt":
			writeJSONResponse(t, w, map[string]any{
				"id_token":     "solo-new-id",
				"access_token": "solo-new-access",
			})
		default:
			http.Error(w, "unexpected refresh token", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv(refreshTokenURLOverrideEnv, server.URL)

	candidates, err := loadCandidates(dir)
	if err != nil {
		t.Fatalf("loadCandidates: %v", err)
	}

	var out bytes.Buffer
	err = refreshAuthAliases(&out, dir, candidates, defaultRefreshMinAgeDays)
	if err == nil {
		t.Fatal("expected aggregate refresh error")
	}
	if !strings.Contains(err.Error(), "refresh failed for 1 auth file(s)") {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	if counts["shared-rt"] != 1 {
		t.Fatalf("expected one shared refresh request, got %d", counts["shared-rt"])
	}
	if counts["solo-rt"] != 1 {
		t.Fatalf("expected one solo refresh request, got %d", counts["solo-rt"])
	}
	mu.Unlock()

	if got, err := os.ReadFile(filepath.Join(dir, "auth.json")); err != nil {
		t.Fatalf("read active auth: %v", err)
	} else if !bytes.Equal(got, activeBytes) {
		t.Fatalf("active auth should stay unchanged\n got: %s\nwant: %s", got, activeBytes)
	}

	aliasA := readJSONFile(t, filepath.Join(dir, "auth.json.a"))
	tokensA := nestedMap(t, aliasA, "tokens")
	if got := tokensA["id_token"]; got != "shared-new-id" {
		t.Fatalf("unexpected alias a id_token: %#v", got)
	}
	if got := tokensA["access_token"]; got != "shared-new-access" {
		t.Fatalf("unexpected alias a access_token: %#v", got)
	}
	if got := tokensA["refresh_token"]; got != "shared-new-rt" {
		t.Fatalf("unexpected alias a refresh_token: %#v", got)
	}
	if got := tokensA["scope"]; got != "keep-me" {
		t.Fatalf("expected nested unknown field preserved, got %#v", got)
	}
	if got := nestedMap(t, aliasA, "custom_field")["team"]; got != "alpha" {
		t.Fatalf("expected top-level unknown field preserved, got %#v", got)
	}
	if got := aliasA["last_refresh"]; got != fixedNow.Format(time.RFC3339) {
		t.Fatalf("unexpected alias a last_refresh: %#v", got)
	}

	aliasB := readJSONFile(t, filepath.Join(dir, "auth.json.b"))
	tokensB := nestedMap(t, aliasB, "tokens")
	if got := tokensB["refresh_token"]; got != "shared-new-rt" {
		t.Fatalf("unexpected alias b refresh_token: %#v", got)
	}
	if got := tokensB["account_id"]; got != "acct-1" {
		t.Fatalf("expected account_id preserved, got %#v", got)
	}

	aliasSolo := readJSONFile(t, filepath.Join(dir, "auth.json.solo"))
	tokensSolo := nestedMap(t, aliasSolo, "tokens")
	if got := tokensSolo["access_token"]; got != "solo-new-access" {
		t.Fatalf("unexpected solo access_token: %#v", got)
	}
	if got := tokensSolo["id_token"]; got != "solo-new-id" {
		t.Fatalf("unexpected solo id_token: %#v", got)
	}
	if got := tokensSolo["refresh_token"]; got != "solo-rt" {
		t.Fatalf("expected solo refresh_token to stay unchanged, got %#v", got)
	}

	output := out.String()
	for _, want := range []string{
		"[refreshed] auth.json.a: last_refresh=<1m",
		"[refreshed] auth.json.b: last_refresh=<1m",
		"[refreshed] auth.json.solo: last_refresh=<1m",
		"[skipped] auth.json.apikey: auth_mode resolves to \"apikey\"",
		"[skipped] auth.json.notokens: auth.json does not contain tokens",
		"[skipped] auth.json.norefresh: auth.json does not contain a refresh_token",
		"[failed] auth.json.bad:",
		"Summary: 3 refreshed, 3 skipped, 1 failed",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q\nfull output:\n%s", want, output)
		}
	}
}

func TestRefreshAuthAliasesClassifiesPermanent401Failures(t *testing.T) {
	dir := t.TempDir()
	writeJSONFile(t, filepath.Join(dir, "auth.json.revoked"), map[string]any{
		"tokens": map[string]any{
			"id_token":      "old-id",
			"access_token":  "old-access",
			"refresh_token": "revoked-rt",
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSONResponse(t, w, map[string]any{
			"error": map[string]any{
				"code": "refresh_token_invalidated",
			},
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv(refreshTokenURLOverrideEnv, server.URL)

	candidates, err := loadCandidates(dir)
	if err != nil {
		t.Fatalf("loadCandidates: %v", err)
	}

	var out bytes.Buffer
	err = refreshAuthAliases(&out, dir, candidates, defaultRefreshMinAgeDays)
	if err == nil {
		t.Fatal("expected refresh failure")
	}
	if !strings.Contains(err.Error(), "refresh failed for 1 auth file(s)") {
		t.Fatalf("unexpected error: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, refreshTokenInvalidatedMessage) {
		t.Fatalf("expected revoked message, got output:\n%s", output)
	}

	revoked := readJSONFile(t, filepath.Join(dir, "auth.json.revoked"))
	if _, ok := revoked["last_refresh"]; ok {
		t.Fatalf("revoked auth should not gain last_refresh: %#v", revoked)
	}
}

func TestRefreshAuthAliasesHonorsLastRefreshThresholdAndSharedTokenGroups(t *testing.T) {
	dir := t.TempDir()

	fixedNow := time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	writeJSONFile(t, filepath.Join(dir, "auth.json.old"), map[string]any{
		"tokens": map[string]any{
			"id_token":      "old-id",
			"access_token":  "old-access",
			"refresh_token": "shared-rt",
		},
		"last_refresh": "2026-04-01T09:00:00Z",
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.recent-shared"), map[string]any{
		"tokens": map[string]any{
			"id_token":      "recent-shared-id",
			"access_token":  "recent-shared-access",
			"refresh_token": "shared-rt",
		},
		"last_refresh": "2026-04-08T09:00:00Z",
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.recent-solo"), map[string]any{
		"tokens": map[string]any{
			"id_token":      "recent-solo-id",
			"access_token":  "recent-solo-access",
			"refresh_token": "recent-rt",
		},
		"last_refresh": "2026-04-08T09:00:00Z",
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.missing"), map[string]any{
		"tokens": map[string]any{
			"id_token":      "missing-id",
			"access_token":  "missing-access",
			"refresh_token": "missing-rt",
		},
	})

	var (
		mu     sync.Mutex
		counts = map[string]int{}
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		mu.Lock()
		counts[req.RefreshToken]++
		mu.Unlock()

		switch req.RefreshToken {
		case "shared-rt":
			writeJSONResponse(t, w, map[string]any{
				"id_token":      "shared-new-id",
				"access_token":  "shared-new-access",
				"refresh_token": "shared-new-rt",
			})
		case "missing-rt":
			writeJSONResponse(t, w, map[string]any{
				"id_token":      "missing-new-id",
				"access_token":  "missing-new-access",
				"refresh_token": "missing-new-rt",
			})
		default:
			t.Fatalf("unexpected refresh token: %s", req.RefreshToken)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv(refreshTokenURLOverrideEnv, server.URL)

	candidates, err := loadCandidates(dir)
	if err != nil {
		t.Fatalf("loadCandidates: %v", err)
	}

	var out bytes.Buffer
	if err := refreshAuthAliases(&out, dir, candidates, defaultRefreshMinAgeDays); err != nil {
		t.Fatalf("refreshAuthAliases: %v", err)
	}

	mu.Lock()
	if counts["shared-rt"] != 1 {
		t.Fatalf("expected one shared refresh request, got %d", counts["shared-rt"])
	}
	if counts["missing-rt"] != 1 {
		t.Fatalf("expected one missing refresh request, got %d", counts["missing-rt"])
	}
	if counts["recent-rt"] != 0 {
		t.Fatalf("expected recent token to be skipped, got %d", counts["recent-rt"])
	}
	mu.Unlock()

	oldAlias := readJSONFile(t, filepath.Join(dir, "auth.json.old"))
	if got := nestedMap(t, oldAlias, "tokens")["refresh_token"]; got != "shared-new-rt" {
		t.Fatalf("unexpected old alias refresh_token: %#v", got)
	}

	recentShared := readJSONFile(t, filepath.Join(dir, "auth.json.recent-shared"))
	if got := nestedMap(t, recentShared, "tokens")["refresh_token"]; got != "shared-new-rt" {
		t.Fatalf("unexpected recent shared alias refresh_token: %#v", got)
	}

	recentSolo := readJSONFile(t, filepath.Join(dir, "auth.json.recent-solo"))
	if got := nestedMap(t, recentSolo, "tokens")["refresh_token"]; got != "recent-rt" {
		t.Fatalf("recent solo alias should be unchanged, got %#v", got)
	}
	if got := recentSolo["last_refresh"]; got != "2026-04-08T09:00:00Z" {
		t.Fatalf("recent solo last_refresh should remain unchanged, got %#v", got)
	}

	missingAlias := readJSONFile(t, filepath.Join(dir, "auth.json.missing"))
	if got := nestedMap(t, missingAlias, "tokens")["refresh_token"]; got != "missing-new-rt" {
		t.Fatalf("unexpected missing alias refresh_token: %#v", got)
	}

	output := out.String()
	for _, want := range []string{
		"[refreshed] auth.json.old: last_refresh=<1m",
		"[refreshed] auth.json.recent-shared: last_refresh=<1m",
		"[refreshed] auth.json.missing: last_refresh=<1m",
		"[skipped] auth.json.recent-solo: last_refresh=2d is newer than 7d threshold",
		"Summary: 3 refreshed, 1 skipped, 0 failed",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q\nfull output:\n%s", want, output)
		}
	}
}

func TestRefreshAuthAliasesReportsWriteFailures(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "auth.json.writefail")
	writeJSONFile(t, targetPath, map[string]any{
		"tokens": map[string]any{
			"id_token":      "old-id",
			"access_token":  "old-access",
			"refresh_token": "writefail-rt",
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.Remove(targetPath); err != nil {
			t.Fatalf("remove target file: %v", err)
		}
		if err := os.Mkdir(targetPath, 0o755); err != nil {
			t.Fatalf("replace target with directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(targetPath, "blocker.txt"), []byte("block"), 0o600); err != nil {
			t.Fatalf("make replacement directory non-empty: %v", err)
		}
		writeJSONResponse(t, w, map[string]any{
			"id_token":      "new-id",
			"access_token":  "new-access",
			"refresh_token": "new-rt",
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv(refreshTokenURLOverrideEnv, server.URL)

	candidates, err := loadCandidates(dir)
	if err != nil {
		t.Fatalf("loadCandidates: %v", err)
	}

	var out bytes.Buffer
	err = refreshAuthAliases(&out, dir, candidates, defaultRefreshMinAgeDays)
	if err == nil {
		t.Fatal("expected write failure")
	}
	if !strings.Contains(err.Error(), "refresh failed for 1 auth file(s)") {
		t.Fatalf("unexpected error: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "[failed] auth.json.writefail: persist") {
		t.Fatalf("expected persist failure, got output:\n%s", output)
	}
}

func writeJSONFile(t *testing.T, path string, payload any) []byte {
	t.Helper()

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return data
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}

	return payload
}

func nestedMap(t *testing.T, payload map[string]any, key string) map[string]any {
	t.Helper()

	value, ok := payload[key].(map[string]any)
	if !ok {
		t.Fatalf("expected %s to be an object, got %#v", key, payload[key])
	}
	return value
}

func writeJSONResponse(t *testing.T, w io.Writer, payload any) {
	t.Helper()

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
