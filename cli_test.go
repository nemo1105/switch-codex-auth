package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunCLIBareCommandUsesInteractiveModeWithoutUsageByDefault(t *testing.T) {
	fixedNow := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeJSONFile(t, filepath.Join(dir, "auth.json"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"email": "demo@example.com", "chatgpt_plan_type": "plus"}),
			"access_token": "interactive-token",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"email": "demo@example.com", "chatgpt_plan_type": "plus"}),
			"access_token": "interactive-token",
		},
	})

	usageRequested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usageRequested = true
		t.Fatalf("usage request should not be sent by default")
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	var out bytes.Buffer
	if err := runCLI(nil, strings.NewReader("demo\n"), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}
	if usageRequested {
		t.Fatal("usage request was sent")
	}

	output := out.String()
	if strings.Contains(output, "Current auth:") {
		t.Fatalf("did not expect current auth line, got:\n%s", output)
	}
	if !strings.Contains(output, " * 1 demo") {
		t.Fatalf("expected current alias marker in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Usage") || !strings.Contains(output, " -") {
		t.Fatalf("expected empty usage display in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Choose auth file by number or suffix: ") {
		t.Fatalf("expected interactive prompt, got:\n%s", output)
	}
	if strings.Contains(output, "[default: demo]") {
		t.Fatalf("did not expect default prompt without usage, got:\n%s", output)
	}
	if !strings.Contains(output, "Already using: demo") {
		t.Fatalf("expected explicit suffix to select the profile, got:\n%s", output)
	}
}

func TestRunCLIBareCommandEnterSwitchesToDefaultCandidate(t *testing.T) {
	fixedNow := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeJSONFile(t, filepath.Join(dir, "auth.json"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"email": "active@example.com", "chatgpt_plan_type": "pro"}),
			"access_token": "active-token",
		},
	})
	bestBytes := writeJSONFile(t, filepath.Join(dir, "auth.json.best"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"email": "best@example.com", "chatgpt_plan_type": "pro"}),
			"access_token": "best-token",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.low"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"email": "low@example.com", "chatgpt_plan_type": "pro"}),
			"access_token": "low-token",
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usedPercent := 60.0
		if strings.TrimSpace(r.Header.Get("Authorization")) == "Bearer best-token" {
			usedPercent = 20
		}
		writeUsageProbeResponse(t, w, map[string]string{
			"x-codex-primary-used-percent":     fmt.Sprint(usedPercent),
			"x-codex-primary-window-minutes":   fmt.Sprint(int64(5 * time.Hour / time.Minute)),
			"x-codex-primary-reset-at":         fmt.Sprint(fixedNow.Add(2 * time.Hour).Unix()),
			"x-codex-secondary-used-percent":   "10",
			"x-codex-secondary-window-minutes": fmt.Sprint(int64(7 * 24 * time.Hour / time.Minute)),
			"x-codex-secondary-reset-at":       fmt.Sprint(fixedNow.Add(6 * 24 * time.Hour).Unix()),
		})
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	var out bytes.Buffer
	if err := runCLI([]string{"--usage", "chat"}, strings.NewReader("\n"), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	activeBytes, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("read active auth: %v", err)
	}
	if !bytes.Equal(activeBytes, bestBytes) {
		t.Fatalf("empty input should switch to best default profile\n got: %s\nwant: %s", activeBytes, bestBytes)
	}

	output := out.String()
	if !strings.Contains(output, "Choose auth file by number or suffix [default: best]: ") {
		t.Fatalf("expected best default prompt, got:\n%s", output)
	}
	if !strings.Contains(output, "Switched auth to: best") {
		t.Fatalf("expected switch confirmation, got:\n%s", output)
	}
}

func TestRunCLIBareCommandRefreshesStaleAliasesInBackgroundAndSyncsSelectedAuth(t *testing.T) {
	fixedNow := time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	setUsageBaseURLForTest(t, "")

	writeJSONFile(t, filepath.Join(dir, "auth.json"), map[string]any{
		"tokens": map[string]any{
			"refresh_token": "active-rt",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{
			"refresh_token": "demo-rt",
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)

		var req refreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.RefreshToken != "demo-rt" {
			t.Fatalf("unexpected refresh token: %s", req.RefreshToken)
		}

		writeJSONResponse(t, w, map[string]any{
			"id_token":      "new-id",
			"access_token":  "new-access",
			"refresh_token": "new-rt",
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv(refreshTokenURLOverrideEnv, server.URL)

	var out bytes.Buffer
	if err := runCLI(nil, strings.NewReader("demo\n"), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	active := readJSONFile(t, filepath.Join(dir, "auth.json"))
	activeTokens := nestedMap(t, active, "tokens")
	if got := activeTokens["access_token"]; got != "new-access" {
		t.Fatalf("expected active auth to be synced after refresh, got %#v", got)
	}
	if got := activeTokens["refresh_token"]; got != "new-rt" {
		t.Fatalf("expected active refresh token to be synced after refresh, got %#v", got)
	}

	output := out.String()
	for _, want := range []string{
		"Refreshing stale auth aliases in background: demo",
		"Switched auth to: demo",
		"Auth refresh is still running; waiting for it to finish...",
		"Auth refresh results:",
		"[refreshed] auth.json.demo: last_refresh=<1m",
		"Updated active auth from refreshed profile: demo",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, output)
		}
	}

	switchIndex := strings.Index(output, "Switched auth to: demo")
	waitIndex := strings.Index(output, "Auth refresh is still running; waiting for it to finish...")
	if switchIndex < 0 || waitIndex < 0 || waitIndex < switchIndex {
		t.Fatalf("expected switch confirmation before refresh wait message, got:\n%s", output)
	}
}

func TestRunCLIBareCommandDoesNotStartBackgroundRefreshWithoutStaleAliases(t *testing.T) {
	fixedNow := time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	setUsageBaseURLForTest(t, "")

	writeJSONFile(t, filepath.Join(dir, "auth.json"), map[string]any{
		"tokens": map[string]any{
			"refresh_token": "active-rt",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{
			"access_token":  "old-access",
			"refresh_token": "demo-rt",
		},
		"last_refresh": fixedNow.Add(-24 * time.Hour).Format(time.RFC3339),
	})

	var out bytes.Buffer
	if err := runCLI(nil, strings.NewReader("demo\n"), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	output := out.String()
	for _, unwanted := range []string{
		"Refreshing stale auth aliases in background:",
		"Auth refresh results:",
		"Auth refresh is still running",
	} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("did not expect background refresh output %q, got:\n%s", unwanted, output)
		}
	}

	active := readJSONFile(t, filepath.Join(dir, "auth.json"))
	activeTokens := nestedMap(t, active, "tokens")
	if got := activeTokens["access_token"]; got != "old-access" {
		t.Fatalf("expected active auth to be copied without refresh, got %#v", got)
	}
}

func TestRunCLIListCommandDoesNotRequestUsageByDefault(t *testing.T) {
	fixedNow := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeJSONFile(t, filepath.Join(dir, "auth.json"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"email": "demo@example.com", "chatgpt_plan_type": "pro"}),
			"access_token": "demo-token",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"email": "demo@example.com", "chatgpt_plan_type": "pro"}),
			"access_token": "demo-token",
		},
	})

	usageRequested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usageRequested = true
		t.Fatalf("usage request should not be sent by default")
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	var out bytes.Buffer
	if err := runCLI([]string{"list"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}
	if usageRequested {
		t.Fatal("usage request was sent")
	}

	output := out.String()
	for _, want := range []string{
		"Available auth files (1):",
		"Usage",
		" * 1 demo",
		" -",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected list output to contain %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Pro |") {
		t.Fatalf("did not expect usage summary without --usage, got:\n%s", output)
	}
}

func TestRunCLIListCommandDisplaysChatUsage(t *testing.T) {
	fixedNow := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeJSONFile(t, filepath.Join(dir, "auth.json"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"email": "demo@example.com", "chatgpt_plan_type": "pro"}),
			"access_token": "demo-token",
			"account_id":   "acct-demo",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"email": "demo@example.com", "chatgpt_plan_type": "pro"}),
			"access_token": "demo-token",
			"account_id":   "acct-demo",
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertUsageProbeRequest(t, r, "Bearer demo-token", "acct-demo")
		writeUsageProbeResponse(t, w, map[string]string{
			"x-codex-primary-used-percent":   "42",
			"x-codex-primary-window-minutes": "5",
			"x-codex-primary-reset-at":       fmt.Sprint(fixedNow.Add(3 * time.Minute).Unix()),
			"x-codex-credits-has-credits":    "true",
			"x-codex-credits-unlimited":      "false",
			"x-codex-credits-balance":        "9.99",
		})
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	var out bytes.Buffer
	if err := runCLI([]string{"list", "--usage", "chat"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	output := out.String()
	for _, want := range []string{
		"Available auth files (1):",
		"Usage",
		" * 1 demo",
		"Pro |  58% left in     3m | Credits 9.99",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected list output to contain %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Current auth:") {
		t.Fatalf("did not expect current auth line, got:\n%s", output)
	}
	for _, unwanted := range []string{"Modified", "Accessed"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("expected list output not to contain %q, got:\n%s", unwanted, output)
		}
	}
	if strings.Contains(output, "Choose auth file by number or suffix") {
		t.Fatalf("list command should not prompt, got:\n%s", output)
	}
}

func TestRunCLIListCommandDisplaysAPIUsage(t *testing.T) {
	fixedNow := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeJSONFile(t, filepath.Join(dir, "auth.json"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"email": "api@example.com", "chatgpt_plan_type": "plus"}),
			"access_token": "api-token",
			"account_id":   "acct-api",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.api"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"email": "api@example.com", "chatgpt_plan_type": "plus"}),
			"access_token": "api-token",
			"account_id":   "acct-api",
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertUsageAPIRequest(t, r, "Bearer api-token", "acct-api")
		writeJSONResponse(t, w, map[string]any{
			"plan_type": "plus",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         20,
					"limit_window_seconds": 5 * 60,
					"reset_at":             fixedNow.Add(4 * time.Minute).Unix(),
				},
			},
		})
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	var out bytes.Buffer
	if err := runCLI([]string{"list", "--usage", "api"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Plus |  80% left in     4m") {
		t.Fatalf("expected API usage summary, got:\n%s", output)
	}
}

func TestRunCLIUseCommandSwitchesBySuffix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	setUsageBaseURLForTest(t, "")

	writeAuthFile(t, filepath.Join(dir, "auth.json"), "active-auth")
	writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "demo-auth")

	var out bytes.Buffer
	if err := runCLI([]string{"use", "demo"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	if got := readAuthFile(t, filepath.Join(dir, "auth.json")); got != "demo-auth" {
		t.Fatalf("unexpected active auth after switch: %q", got)
	}
	if output := out.String(); !strings.Contains(output, "Switched auth to: demo") {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestRunCLISaveCommandSupportsForceAfterSuffix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	setUsageBaseURLForTest(t, "")

	writeAuthFile(t, filepath.Join(dir, "auth.json"), "active-auth")
	writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "old-demo")

	var out bytes.Buffer
	if err := runCLI([]string{"save", "demo", "--force"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	if got := readAuthFile(t, filepath.Join(dir, "auth.json.demo")); got != "active-auth" {
		t.Fatalf("expected save overwrite, got %q", got)
	}
	if output := out.String(); !strings.Contains(output, "Overwrote auth alias: demo") {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestRunCLIRefreshCommandDispatches(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	setUsageBaseURLForTest(t, "")

	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{
			"id_token":      "old-id",
			"access_token":  "old-access",
			"refresh_token": "demo-rt",
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.RefreshToken != "demo-rt" {
			t.Fatalf("unexpected refresh token: %s", req.RefreshToken)
		}

		writeJSONResponse(t, w, map[string]any{
			"id_token":      "new-id",
			"access_token":  "new-access",
			"refresh_token": "new-rt",
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv(refreshTokenURLOverrideEnv, server.URL)

	var out bytes.Buffer
	if err := runCLI([]string{"refresh"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "[refreshed] auth.json.demo:") {
		t.Fatalf("expected refresh output, got:\n%s", output)
	}
	if !strings.Contains(output, "Summary: 1 refreshed, 0 skipped, 0 failed") {
		t.Fatalf("expected refresh summary, got:\n%s", output)
	}
}

func TestRunCLIRefreshCommandSupportsDaysFlag(t *testing.T) {
	fixedNow := time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	setUsageBaseURLForTest(t, "")

	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{
			"id_token":      "old-id",
			"access_token":  "old-access",
			"refresh_token": "demo-rt",
		},
		"last_refresh": "2026-04-08T09:00:00Z",
	})

	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeJSONResponse(t, w, map[string]any{
			"id_token":      "new-id",
			"access_token":  "new-access",
			"refresh_token": "new-rt",
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv(refreshTokenURLOverrideEnv, server.URL)

	var out bytes.Buffer
	if err := runCLI([]string{"refresh", "--days", "1"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	if !called {
		t.Fatal("expected refresh request to be sent")
	}
	if output := out.String(); !strings.Contains(output, "Summary: 1 refreshed, 0 skipped, 0 failed") {
		t.Fatalf("expected refresh summary, got:\n%s", output)
	}
}

func TestRunCLIRefreshCommandForceIgnoresRecentLastRefresh(t *testing.T) {
	fixedNow := time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	setUsageBaseURLForTest(t, "")

	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{
			"id_token":      "old-id",
			"access_token":  "old-access",
			"refresh_token": "demo-rt",
		},
		"last_refresh": "2026-04-10T08:00:00Z",
	})

	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true

		var req refreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.RefreshToken != "demo-rt" {
			t.Fatalf("unexpected refresh token: %s", req.RefreshToken)
		}

		writeJSONResponse(t, w, map[string]any{
			"id_token":      "new-id",
			"access_token":  "new-access",
			"refresh_token": "new-rt",
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv(refreshTokenURLOverrideEnv, server.URL)

	var out bytes.Buffer
	if err := runCLI([]string{"refresh", "--force"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	if !called {
		t.Fatal("expected refresh request to be sent")
	}
	if output := out.String(); !strings.Contains(output, "Summary: 1 refreshed, 0 skipped, 0 failed") {
		t.Fatalf("expected refresh summary, got:\n%s", output)
	}

	got := readJSONFile(t, filepath.Join(dir, "auth.json.demo"))
	if gotRefreshToken := nestedMap(t, got, "tokens")["refresh_token"]; gotRefreshToken != "new-rt" {
		t.Fatalf("unexpected refresh token: %#v", gotRefreshToken)
	}
}

func TestParseRefreshSubcommandArgsSupportsForceFlags(t *testing.T) {
	for _, args := range [][]string{{"-f"}, {"--force"}} {
		var out bytes.Buffer
		options, handled, err := parseRefreshSubcommandArgs(args, &out, "switch-codex-auth")
		if err != nil {
			t.Fatalf("parseRefreshSubcommandArgs(%v): %v", args, err)
		}
		if handled {
			t.Fatalf("parseRefreshSubcommandArgs(%v) handled unexpectedly", args)
		}
		if !options.Force {
			t.Fatalf("parseRefreshSubcommandArgs(%v) did not set force", args)
		}
	}
}

func TestRunCLIRefreshCommandRejectsNegativeDays(t *testing.T) {
	var out bytes.Buffer
	err := runCLI([]string{"refresh", "--days", "-1"}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("expected negative days error")
	}
	if !strings.Contains(err.Error(), "refresh days must be >= 0") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunCLILegacyFlagsFailWithMigrationGuidance(t *testing.T) {
	var out bytes.Buffer
	err := runCLI([]string{"--use", "demo"}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("expected legacy flag error")
	}
	if !strings.Contains(err.Error(), "legacy action flags are no longer supported") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "use demo") {
		t.Fatalf("expected migration guidance, got: %v", err)
	}
}

func TestRunCLIRejectsInvalidUsageMode(t *testing.T) {
	for _, args := range [][]string{
		{"--usage", "bad"},
		{"list", "--usage", "bad"},
	} {
		var out bytes.Buffer
		err := runCLI(args, strings.NewReader(""), &out)
		if err == nil {
			t.Fatalf("expected invalid usage mode error for %v", args)
		}
		if !strings.Contains(err.Error(), "usage must be one of: none, api, chat") {
			t.Fatalf("unexpected error for %v: %v", args, err)
		}
	}
}

func TestRunCLIRejectsUsageFlagOnUseCommand(t *testing.T) {
	var out bytes.Buffer
	err := runCLI([]string{"use", "--usage", "chat", "demo"}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("expected use --usage error")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunCLIRequiresSaveSuffix(t *testing.T) {
	setUsageBaseURLForTest(t, "")

	var out bytes.Buffer
	err := runCLI([]string{"save"}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("expected missing suffix error")
	}
	if !strings.Contains(err.Error(), "save requires <suffix>") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunCLIUnknownCommandFailsClearly(t *testing.T) {
	setUsageBaseURLForTest(t, "")

	var out bytes.Buffer
	err := runCLI([]string{"wat"}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("expected unknown command error")
	}
	if !strings.Contains(err.Error(), "unknown command: wat") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunCLIHelpCommandShowsSubcommandUsage(t *testing.T) {
	setUsageBaseURLForTest(t, "")

	var out bytes.Buffer
	if err := runCLI([]string{"help", "save"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	if got := out.String(); !strings.Contains(got, "save <suffix> [-f|--force]") {
		t.Fatalf("unexpected help output:\n%s", got)
	}
}

func TestIsInteractiveReader(t *testing.T) {
	if isInteractiveReader(strings.NewReader("")) {
		t.Fatal("strings reader should not be interactive")
	}

	file, err := os.CreateTemp(t.TempDir(), "interactive-reader")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() {
		_ = file.Close()
	})

	if isInteractiveReader(file) {
		t.Fatal("temp file should not be treated as interactive")
	}
}

func setUsageBaseURLForTest(t *testing.T, value string) {
	t.Helper()

	previous := usageBaseURL
	usageBaseURL = value
	t.Cleanup(func() {
		usageBaseURL = previous
	})
}
