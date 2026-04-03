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
)

func TestRunCLIBareCommandUsesInteractiveMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)

	writeAuthFile(t, filepath.Join(dir, "auth.json"), "demo-auth")
	writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "demo-auth")

	var out bytes.Buffer
	if err := runCLI(nil, strings.NewReader("\n"), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Current auth: demo") {
		t.Fatalf("expected current auth in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Choose auth file by number or suffix (Enter to cancel): ") {
		t.Fatalf("expected interactive prompt, got:\n%s", output)
	}
	if !strings.Contains(output, "Cancelled.") {
		t.Fatalf("expected cancel message, got:\n%s", output)
	}
}

func TestRunCLIListCommandDisplaysStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)

	writeAuthFile(t, filepath.Join(dir, "auth.json"), "demo-auth")
	writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "demo-auth")

	var out bytes.Buffer
	if err := runCLI([]string{"list"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Available auth files (1):") {
		t.Fatalf("expected list output, got:\n%s", output)
	}
	if strings.Contains(output, "Choose auth file by number or suffix") {
		t.Fatalf("list command should not prompt, got:\n%s", output)
	}
}

func TestRunCLIUseCommandSwitchesBySuffix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)

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
