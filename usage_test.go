package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFormatUsageSummary(t *testing.T) {
	fixedNow := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	summary := formatUsageSummary([]accountRateLimitSnapshot{
		{
			LimitID:  "codex",
			PlanType: accountPlanTypePro,
			Primary: &accountRateLimitWindow{
				UsedPercent:        42,
				WindowDurationMins: int64Ptr(5),
				ResetsAt:           int64Ptr(fixedNow.Add(3 * time.Minute).Unix()),
			},
			Secondary: &accountRateLimitWindow{
				UsedPercent:        84,
				WindowDurationMins: int64Ptr(60),
				ResetsAt:           int64Ptr(fixedNow.Add(3*24*time.Hour + 21*time.Hour).Unix()),
			},
			Credits: &accountCreditsSnapshot{
				HasCredits: true,
				Balance:    "9.99",
			},
		},
		{
			LimitID: "codex_other",
			Primary: &accountRateLimitWindow{
				UsedPercent:        70,
				WindowDurationMins: int64Ptr(15),
			},
		},
	})

	if want := "Pro |  58% left in     3m |  16% left in  3d21h | Credits 9.99 | +1 extra"; summary != want {
		t.Fatalf("unexpected summary:\n got: %s\nwant: %s", summary, want)
	}
}

func TestFormatUsageSummaryOmitsNoneCredits(t *testing.T) {
	fixedNow := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	summary := formatUsageSummary([]accountRateLimitSnapshot{
		{
			LimitID:  "codex",
			PlanType: accountPlanTypeTeam,
			Primary: &accountRateLimitWindow{
				UsedPercent:        17,
				WindowDurationMins: int64Ptr(300),
				ResetsAt:           int64Ptr(fixedNow.Add(4 * time.Hour).Unix()),
			},
			Secondary: &accountRateLimitWindow{
				UsedPercent:        0,
				WindowDurationMins: int64Ptr(10080),
				ResetsAt:           int64Ptr(fixedNow.Add(3*24*time.Hour + 21*time.Hour).Unix()),
			},
			Credits: &accountCreditsSnapshot{},
		},
	})

	if want := "Team |  83% left in     4h | 100% left in  3d21h"; summary != want {
		t.Fatalf("unexpected summary:\n got: %s\nwant: %s", summary, want)
	}
}

func TestFormatUsageErrorTimeout(t *testing.T) {
	err := fmt.Errorf("perform usage request: %w", context.DeadlineExceeded)
	if got := formatUsageError(err); got != "Request timeout" {
		t.Fatalf("unexpected timeout error: %q", got)
	}
}

func TestDecodeAccountUsagePayloadNormalizesPlanAndExtras(t *testing.T) {
	snapshots, err := decodeAccountUsagePayload([]byte(`{
		"plan_type":"education",
		"rate_limit":{
			"primary_window":{"used_percent":25,"limit_window_seconds":900,"reset_at":111}
		},
		"credits":{"has_credits":false,"unlimited":true},
		"additional_rate_limits":[
			{
				"limit_name":"Codex Other",
				"metered_feature":"codex-other",
				"rate_limit":{
					"primary_window":{"used_percent":12.5,"limit_window_seconds":300,"reset_at":222}
				}
			}
		]
	}`))
	if err != nil {
		t.Fatalf("decodeAccountUsagePayload: %v", err)
	}

	if len(snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snapshots))
	}
	if snapshots[0].PlanType != accountPlanTypeEdu {
		t.Fatalf("unexpected plan type: %q", snapshots[0].PlanType)
	}
	if snapshots[0].Credits == nil || !snapshots[0].Credits.Unlimited {
		t.Fatalf("expected unlimited credits snapshot, got %#v", snapshots[0].Credits)
	}
	if snapshots[1].LimitID != "codex_other" {
		t.Fatalf("unexpected additional limit id: %q", snapshots[1].LimitID)
	}
}

func TestEnrichCandidatesWithUsageDeduplicatesAndHandlesErrors(t *testing.T) {
	fixedNow := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	dir := t.TempDir()

	writeJSONFile(t, filepath.Join(dir, "auth.json.shared-a"), map[string]any{
		"tokens": map[string]any{
			"access_token": "shared-token",
			"account_id":   "acct-1",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.shared-b"), map[string]any{
		"tokens": map[string]any{
			"access_token": "shared-token",
			"account_id":   "acct-1",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.distinct"), map[string]any{
		"tokens": map[string]any{
			"access_token": "distinct-token",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.apikey"), map[string]any{
		"OPENAI_API_KEY": "sk-test",
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.missing-access"), map[string]any{
		"tokens": map[string]any{
			"refresh_token": "rt",
		},
	})
	if err := os.WriteFile(filepath.Join(dir, "auth.json.bad"), []byte(`{"tokens":`), 0o600); err != nil {
		t.Fatalf("write malformed auth: %v", err)
	}

	var (
		mu     sync.Mutex
		counts = map[string]int{}
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		accountID := strings.TrimSpace(r.Header.Get("ChatGPT-Account-ID"))

		mu.Lock()
		counts[authHeader+"|"+accountID]++
		mu.Unlock()

		switch authHeader {
		case "Bearer shared-token":
			if accountID != "acct-1" {
				t.Fatalf("unexpected account id for shared token: %q", accountID)
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
			})
		case "Bearer distinct-token":
			w.WriteHeader(http.StatusUnauthorized)
			writeJSONResponse(t, w, map[string]any{
				"error": map[string]any{
					"message": "Provided authentication token is expired. Please try signing in again.",
					"code":    "token_expired",
				},
				"status": http.StatusUnauthorized,
			})
		default:
			t.Fatalf("unexpected auth header: %q", authHeader)
		}
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	candidates, err := loadCandidates(dir)
	if err != nil {
		t.Fatalf("loadCandidates: %v", err)
	}

	enriched := enrichCandidatesWithUsage(candidates)
	bySuffix := make(map[string]candidate, len(enriched))
	for _, candidate := range enriched {
		bySuffix[candidate.Suffix] = candidate
	}

	if got := bySuffix["shared-a"].Usage; got != "Pro |  58% left in     3m" {
		t.Fatalf("unexpected shared-a usage: %q", got)
	}
	if got := bySuffix["shared-b"].Usage; got != "Pro |  58% left in     3m" {
		t.Fatalf("unexpected shared-b usage: %q", got)
	}
	if got := bySuffix["distinct"].Usage; got != "401: Provided authentication token is expired. Please try signing in again." {
		t.Fatalf("unexpected distinct usage: %q", got)
	}
	if got := bySuffix["apikey"].Usage; got != "n/a" {
		t.Fatalf("unexpected apikey usage: %q", got)
	}
	if got := bySuffix["missing-access"].Usage; got != "n/a" {
		t.Fatalf("unexpected missing-access usage: %q", got)
	}
	if got := bySuffix["bad"].Usage; !strings.Contains(got, "parse ") || !strings.Contains(got, "auth.json.bad") || !strings.Contains(got, "unexpected end of JSON input") {
		t.Fatalf("unexpected malformed usage: %q", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if counts["Bearer shared-token|acct-1"] != 1 {
		t.Fatalf("expected one shared usage request, got %d", counts["Bearer shared-token|acct-1"])
	}
	if counts["Bearer distinct-token|"] != 1 {
		t.Fatalf("expected one distinct usage request, got %d", counts["Bearer distinct-token|"])
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}
