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
				},
				{
					Suffix: "wcl",
					Metadata: authMetadata{
						ModTime: baseMod.Add(-time.Hour),
					},
				},
			},
			wantContains: []string{
				"Auth dir: /tmp/.codex",
				"Current auth: 09",
				"Available auth files (2):",
				"Current",
				"Accessed",
				"Last refresh",
				"36m ago",
				"3m ago",
				"37m ago",
				"Hint: newer last_refresh and access times usually mean more recent activity",
			},
			wantMissing: []string{
				"Current auth details:",
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
				{Suffix: "09"},
			},
			wantContains: []string{
				"Current auth details: mtime=36m ago, atime=3m ago, last_refresh=37m ago",
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
