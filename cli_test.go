package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunCLIBareCommandUsesInteractiveMode(t *testing.T) {
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
			"access_token": "interactive-token",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{
			"access_token": "interactive-token",
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(t, w, map[string]any{
			"plan_type": "plus",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         25,
					"limit_window_seconds": 900,
					"reset_at":             fixedNow.Add(3 * time.Minute).Unix(),
				},
			},
		})
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	var out bytes.Buffer
	if err := runCLI(nil, strings.NewReader("\n"), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	output := out.String()
	if strings.Contains(output, "Current auth:") {
		t.Fatalf("did not expect current auth line, got:\n%s", output)
	}
	if !strings.Contains(output, " * 1 demo") {
		t.Fatalf("expected current alias marker in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Plus |  75% left in     3m") {
		t.Fatalf("expected usage summary in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Choose auth file by number or suffix (Enter to cancel): ") {
		t.Fatalf("expected interactive prompt, got:\n%s", output)
	}
	if !strings.Contains(output, "Cancelled.") {
		t.Fatalf("expected cancel message, got:\n%s", output)
	}
}

func TestRunCLIListCommandDisplaysStatus(t *testing.T) {
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
			"access_token": "demo-token",
			"account_id":   "acct-demo",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{
			"access_token": "demo-token",
			"account_id":   "acct-demo",
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer demo-token" {
			t.Fatalf("unexpected auth header: %s", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-demo" {
			t.Fatalf("unexpected account header: %s", got)
		}
		if got := r.Header.Get("originator"); got == "" {
			t.Fatal("expected originator header")
		}
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Fatal("expected User-Agent header")
		}
		if got := r.Header.Get("version"); got == "" {
			t.Fatal("expected version header")
		}
		writeJSONResponse(t, w, map[string]any{
			"plan_type": "pro",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         42,
					"limit_window_seconds": 300,
					"reset_at":             fixedNow.Add(3 * time.Minute).Unix(),
				},
			},
			"credits": map[string]any{
				"has_credits": true,
				"unlimited":   false,
				"balance":     "9.99",
			},
		})
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	var out bytes.Buffer
	if err := runCLI([]string{"list"}, strings.NewReader(""), &out); err != nil {
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
