package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPrintStatus(t *testing.T) {
	fixedNow := time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	baseMod := fixedNow.Add(-36 * time.Minute)
	baseAccess := fixedNow.Add(-3 * time.Minute)
	baseRefresh := fixedNow.Add(-37 * time.Minute)

	tests := []struct {
		name            string
		current         string
		currentMetadata *authMetadata
		candidates      []candidate
		wantContains    []string
		wantMissing     []string
	}{
		{
			name:    "renders metadata table",
			current: "09",
			candidates: []candidate{
				{
					Suffix: "09",
					Metadata: authMetadata{
						ModTime:        baseMod,
						AccessTime:     baseAccess,
						HasAccessTime:  true,
						LastRefresh:    baseRefresh,
						HasLastRefresh: true,
					},
					Usage: "Pro |  58% left in     3m",
				},
				{
					Suffix: "wcl",
					Metadata: authMetadata{
						ModTime: baseMod.Add(-time.Hour),
					},
					Usage: "n/a",
				},
			},
			wantContains: []string{
				"Auth dir: /tmp/.codex",
				"Available auth files (2):",
				"#",
				"Suffix",
				"Last refresh",
				"Usage",
				" * 1",
				"Pro |  58% left in     3m",
				"n/a",
				"37m ago",
				"Hint:",
				"  * marks the current alias",
				"  Last refresh is a local file signal",
				"  Usage is live account data and may show n/a or a concise error per alias",
			},
			wantMissing: []string{
				"Current auth:",
				"Current auth details:",
				"Modified",
				"Accessed",
			},
		},
		{
			name:    "shows current details for unmatched auth",
			current: "custom/unmatched",
			currentMetadata: &authMetadata{
				ModTime:        baseMod,
				AccessTime:     baseAccess,
				HasAccessTime:  true,
				LastRefresh:    baseRefresh,
				HasLastRefresh: true,
			},
			candidates: []candidate{
				{Suffix: "09", Usage: "401: Provided authentication token is expired. Please try signing in again."},
			},
			wantContains: []string{
				"Current auth details: mtime=36m ago, atime=3m ago, last_refresh=37m ago",
				"401: Provided authentication token is expired. Please try signing in again.",
			},
		},
		{
			name:       "shows empty list",
			current:    "none",
			candidates: nil,
			wantContains: []string{
				"Available auth files (0):",
				"  none",
			},
			wantMissing: []string{
				"Hint:",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			printStatus(&out, "/tmp/.codex", tt.current, tt.currentMetadata, tt.candidates)

			output := out.String()
			for _, want := range tt.wantContains {
				if !strings.Contains(output, want) {
					t.Fatalf("output missing %q\nfull output:\n%s", want, output)
				}
			}
			for _, unwanted := range tt.wantMissing {
				if strings.Contains(output, unwanted) {
					t.Fatalf("output unexpectedly contains %q\nfull output:\n%s", unwanted, output)
				}
			}
		})
	}
}

func TestLoadCandidatesSortsAndLoadsMetadata(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "auth.json.wcl"), []byte(`{"last_refresh":"2026-03-31T09:21:03.730713Z"}`), 0o600); err != nil {
		t.Fatalf("write wcl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json.09"), []byte(`{"last_refresh":"2026-03-31T09:23:25.384956Z"}`), 0o600); err != nil {
		t.Fatalf("write 09: %v", err)
	}

	candidates, err := loadCandidates(dir)
	if err != nil {
		t.Fatalf("loadCandidates: %v", err)
	}

	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].Suffix != "09" || candidates[1].Suffix != "wcl" {
		t.Fatalf("unexpected candidate order: %#v", candidates)
	}
	if !candidates[0].Metadata.HasLastRefresh || !candidates[1].Metadata.HasLastRefresh {
		t.Fatalf("expected last_refresh metadata for all candidates: %#v", candidates)
	}
}

func TestLoadAuthMetadataIgnoresInvalidLastRefreshAndKeepsCapturedTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json.bad")
	if err := os.WriteFile(path, []byte(`{"last_refresh":"not-a-time"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accessTime := time.Date(2026, 3, 31, 8, 0, 0, 0, time.UTC)
	modTime := time.Date(2026, 3, 31, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, accessTime, modTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	metadata := loadAuthMetadata(path, info)
	if metadata.ModTime.Unix() != modTime.Unix() {
		t.Fatalf("unexpected mod time: got %v want %v", metadata.ModTime, modTime)
	}
	if metadata.HasLastRefresh {
		t.Fatalf("expected invalid last_refresh to be ignored: %#v", metadata)
	}

	supportsAccessTime := runtime.GOOS == "darwin" || runtime.GOOS == "linux" || runtime.GOOS == "windows"
	if supportsAccessTime && !metadata.HasAccessTime {
		t.Fatalf("expected access time on %s", runtime.GOOS)
	}
	if metadata.HasAccessTime && metadata.AccessTime.Unix() != accessTime.Unix() {
		t.Fatalf("unexpected access time: got %v want %v", metadata.AccessTime, accessTime)
	}
}

func TestFormatDisplayTimeFallback(t *testing.T) {
	fixedNow := time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	if got := formatDisplayTime(time.Time{}, false); got != "-" {
		t.Fatalf("unexpected fallback format: %q", got)
	}

	if got := formatDisplayTime(fixedNow.Add(-3*time.Hour), true); got != "3h ago" {
		t.Fatalf("unexpected past format: %q", got)
	}

	if got := formatDisplayTime(fixedNow.Add(3*24*time.Hour), true); got != "in 3d" {
		t.Fatalf("unexpected future format: %q", got)
	}
}

func TestSaveCurrentAsWithIOSavesNewAlias(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, filepath.Join(dir, "auth.json"), "active")

	var out bytes.Buffer
	if err := saveCurrentAsWithIO(dir, "demo", false, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("saveCurrentAsWithIO: %v", err)
	}

	if got := readAuthFile(t, filepath.Join(dir, "auth.json.demo")); got != "active" {
		t.Fatalf("unexpected saved content: %q", got)
	}
	if output := out.String(); !strings.Contains(output, "Saved current auth as: demo") {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestSaveCurrentAsWithIOPromptsAndOverwritesOnEmptyInput(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, filepath.Join(dir, "auth.json"), "active")
	targetPath := filepath.Join(dir, "auth.json.demo")
	writeAuthFile(t, targetPath, "old")

	var out bytes.Buffer
	if err := saveCurrentAsWithIO(dir, "demo", false, strings.NewReader("\n"), &out, true); err != nil {
		t.Fatalf("saveCurrentAsWithIO: %v", err)
	}

	if got := readAuthFile(t, targetPath); got != "active" {
		t.Fatalf("expected overwrite, got %q", got)
	}

	output := out.String()
	if !strings.Contains(output, "Auth alias already exists: auth.json.demo") {
		t.Fatalf("missing conflict output: %s", output)
	}
	if !strings.Contains(output, "Press Enter to overwrite, or type a different alias to save as: ") {
		t.Fatalf("missing overwrite prompt: %s", output)
	}
	if !strings.Contains(output, "Overwrote auth alias: demo") {
		t.Fatalf("missing overwrite confirmation: %s", output)
	}
}

func TestSaveCurrentAsWithIOUsesReplacementAlias(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, filepath.Join(dir, "auth.json"), "active")
	writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "old-demo")

	var out bytes.Buffer
	if err := saveCurrentAsWithIO(dir, "demo", false, strings.NewReader("backup2\n"), &out, true); err != nil {
		t.Fatalf("saveCurrentAsWithIO: %v", err)
	}

	if got := readAuthFile(t, filepath.Join(dir, "auth.json.demo")); got != "old-demo" {
		t.Fatalf("expected original alias to stay unchanged, got %q", got)
	}
	if got := readAuthFile(t, filepath.Join(dir, "auth.json.backup2")); got != "active" {
		t.Fatalf("expected replacement alias content, got %q", got)
	}
	if output := out.String(); !strings.Contains(output, "Saved current auth as: backup2") {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestSaveCurrentAsWithIORepromptsOnConflictingAlias(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, filepath.Join(dir, "auth.json"), "active")
	writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "old-demo")
	writeAuthFile(t, filepath.Join(dir, "auth.json.other"), "old-other")

	var out bytes.Buffer
	if err := saveCurrentAsWithIO(dir, "demo", false, strings.NewReader("other\ncustom\n"), &out, true); err != nil {
		t.Fatalf("saveCurrentAsWithIO: %v", err)
	}

	if got := readAuthFile(t, filepath.Join(dir, "auth.json.custom")); got != "active" {
		t.Fatalf("expected final alias content, got %q", got)
	}

	output := out.String()
	if strings.Count(output, "Auth alias already exists:") != 2 {
		t.Fatalf("expected two conflict prompts, got output:\n%s", output)
	}
	if !strings.Contains(output, "auth.json.other") {
		t.Fatalf("expected follow-up conflict for replacement alias, got output:\n%s", output)
	}
}

func TestSaveCurrentAsWithIORepromptsOnInvalidAlias(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, filepath.Join(dir, "auth.json"), "active")
	writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "old-demo")

	var out bytes.Buffer
	if err := saveCurrentAsWithIO(dir, "demo", false, strings.NewReader("bad/name\ncustom\n"), &out, true); err != nil {
		t.Fatalf("saveCurrentAsWithIO: %v", err)
	}

	if got := readAuthFile(t, filepath.Join(dir, "auth.json.custom")); got != "active" {
		t.Fatalf("expected final alias content, got %q", got)
	}

	output := out.String()
	if !strings.Contains(output, "Invalid alias: suffix cannot contain path separators: bad/name") {
		t.Fatalf("expected invalid alias message, got output:\n%s", output)
	}
}

func TestSaveCurrentAsWithIOForceOverwritesWithoutPrompt(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, filepath.Join(dir, "auth.json"), "active")
	targetPath := filepath.Join(dir, "auth.json.demo")
	writeAuthFile(t, targetPath, "old")

	var out bytes.Buffer
	if err := saveCurrentAsWithIO(dir, "demo", true, strings.NewReader("backup\n"), &out, true); err != nil {
		t.Fatalf("saveCurrentAsWithIO: %v", err)
	}

	if got := readAuthFile(t, targetPath); got != "active" {
		t.Fatalf("expected force overwrite, got %q", got)
	}

	output := out.String()
	if strings.Contains(output, "Press Enter to overwrite") {
		t.Fatalf("force mode should not prompt, got output:\n%s", output)
	}
	if !strings.Contains(output, "Overwrote auth alias: demo") {
		t.Fatalf("missing force overwrite confirmation: %s", output)
	}
}

func TestSaveCurrentAsWithIONonInteractiveConflictRequiresForce(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, filepath.Join(dir, "auth.json"), "active")
	targetPath := filepath.Join(dir, "auth.json.demo")
	writeAuthFile(t, targetPath, "old")

	var out bytes.Buffer
	err := saveCurrentAsWithIO(dir, "demo", false, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "use --force to overwrite or choose a different alias") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := readAuthFile(t, targetPath); got != "old" {
		t.Fatalf("target should remain unchanged, got %q", got)
	}
}

func TestSaveCurrentAsWithIOEOFDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, filepath.Join(dir, "auth.json"), "active")
	targetPath := filepath.Join(dir, "auth.json.demo")
	writeAuthFile(t, targetPath, "old")

	var out bytes.Buffer
	err := saveCurrentAsWithIO(dir, "demo", false, strings.NewReader(""), &out, true)
	if err == nil {
		t.Fatal("expected EOF conflict error")
	}
	if !strings.Contains(err.Error(), "save cancelled before resolving existing alias") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := readAuthFile(t, targetPath); got != "old" {
		t.Fatalf("target should remain unchanged, got %q", got)
	}
}

func writeAuthFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readAuthFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
