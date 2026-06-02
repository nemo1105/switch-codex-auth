package main

import (
	"context"
	"encoding/json"
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

func TestUsageRemainingMetricsFromSnapshots(t *testing.T) {
	metrics := usageRemainingMetricsFromSnapshots([]accountRateLimitSnapshot{
		{
			LimitID: "codex",
			Primary: &accountRateLimitWindow{
				UsedPercent:        25,
				WindowDurationMins: int64Ptr(fiveHourUsageWindowMins),
			},
			Secondary: &accountRateLimitWindow{
				UsedPercent:        60,
				WindowDurationMins: int64Ptr(sevenDayUsageWindowMins),
			},
		},
	})

	if !metrics.HasFiveHourRemaining || metrics.FiveHourRemaining != 75 {
		t.Fatalf("unexpected five hour remaining metrics: %#v", metrics)
	}
	if !metrics.HasSevenDayRemaining || metrics.SevenDayRemaining != 40 {
		t.Fatalf("unexpected seven day remaining metrics: %#v", metrics)
	}
}

func TestUsageRemainingMetricsIgnoreMismatchedWindows(t *testing.T) {
	metrics := usageRemainingMetricsFromSnapshots([]accountRateLimitSnapshot{
		{
			LimitID: "codex",
			Primary: &accountRateLimitWindow{
				UsedPercent:        25,
				WindowDurationMins: int64Ptr(15),
			},
			Secondary: &accountRateLimitWindow{
				UsedPercent:        60,
				WindowDurationMins: int64Ptr(60),
			},
		},
	})

	if metrics.HasFiveHourRemaining || metrics.HasSevenDayRemaining {
		t.Fatalf("expected mismatched windows to be ignored: %#v", metrics)
	}
}

func TestUsageRemainingMetricsAcceptMissingWindowDurations(t *testing.T) {
	metrics := usageRemainingMetricsFromSnapshots([]accountRateLimitSnapshot{
		{
			LimitID: "codex",
			Primary: &accountRateLimitWindow{
				UsedPercent: 30,
			},
			Secondary: &accountRateLimitWindow{
				UsedPercent: 80,
			},
		},
	})

	if !metrics.HasFiveHourRemaining || metrics.FiveHourRemaining != 70 {
		t.Fatalf("unexpected five hour remaining metrics: %#v", metrics)
	}
	if !metrics.HasSevenDayRemaining || metrics.SevenDayRemaining != 20 {
		t.Fatalf("unexpected seven day remaining metrics: %#v", metrics)
	}
}

func TestMapRateLimitHeaderWindowIgnoresZeroOnlyWindow(t *testing.T) {
	headers := make(http.Header)
	headers.Set("x-codex-primary-used-percent", "0")

	if got := mapRateLimitHeaderWindow(headers, "x-codex-primary"); got != nil {
		t.Fatalf("expected zero-only header window to be ignored, got %#v", got)
	}

	headers.Set("x-codex-primary-window-minutes", "0")
	if got := mapRateLimitHeaderWindow(headers, "x-codex-primary"); got != nil {
		t.Fatalf("expected zero duration header window to be ignored, got %#v", got)
	}

	headers.Set("x-codex-primary-window-minutes", fmt.Sprint(fiveHourUsageWindowMins))
	got := mapRateLimitHeaderWindow(headers, "x-codex-primary")
	if got == nil {
		t.Fatal("expected duration-bearing zero usage window")
	}
	if got.UsedPercent != 0 || got.WindowDurationMins == nil || *got.WindowDurationMins != fiveHourUsageWindowMins {
		t.Fatalf("unexpected duration-bearing zero usage window: %#v", got)
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

func TestExtractPlanTypeFromAuthReadsNestedAuthClaim(t *testing.T) {
	auth := authPayload{
		Tokens: &tokenData{
			IDToken: testJWT(t, map[string]any{
				"https://api.openai.com/auth": map[string]any{
					"chatgpt_plan_type": "plus",
				},
			}),
		},
	}

	if got := extractPlanTypeFromAuth(auth); got != accountPlanTypePlus {
		t.Fatalf("unexpected plan type: %q", got)
	}
}

func TestEnrichCandidatesWithUsageNoneDoesNotRequest(t *testing.T) {
	dir := t.TempDir()
	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"chatgpt_plan_type": "pro"}),
			"access_token": "demo-token",
		},
	})

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		t.Fatalf("usage request should not be sent in none mode")
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	candidates, err := loadCandidates(dir)
	if err != nil {
		t.Fatalf("loadCandidates: %v", err)
	}

	enriched := enrichCandidatesWithUsage(candidates, usageModeNone)
	if called {
		t.Fatal("usage request was sent")
	}
	if got := enriched[0].Usage; got != "" {
		t.Fatalf("none mode should leave usage empty, got %q", got)
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
			"id_token":     testJWT(t, map[string]any{"chatgpt_plan_type": "pro"}),
			"access_token": "shared-token",
			"account_id":   "acct-1",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.shared-b"), map[string]any{
		"tokens": map[string]any{
			"id_token":     testJWT(t, map[string]any{"chatgpt_plan_type": "pro"}),
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
			assertUsageProbeRequest(t, r, "Bearer shared-token", "acct-1")
			writeUsageProbeResponse(t, w, map[string]string{
				"x-codex-primary-used-percent":   "42",
				"x-codex-primary-window-minutes": "5",
				"x-codex-primary-reset-at":       fmt.Sprint(fixedNow.Add(3 * time.Minute).Unix()),
			})
		case "Bearer distinct-token":
			assertUsageProbeRequest(t, r, "Bearer distinct-token", "")
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

	enriched := enrichCandidatesWithUsage(candidates, usageModeChat)
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

func TestRequestAccountUsageViaAPIReadsUsagePayload(t *testing.T) {
	fixedNow := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertUsageAPIRequest(t, r, "Bearer api-token", "acct-api")
		writeJSONResponse(t, w, map[string]any{
			"plan_type": "pro",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         42,
					"limit_window_seconds": 5 * 60,
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

	snapshots, err := requestAccountUsageViaAPI(server.Client(), usageAuth{
		AccessToken: "api-token",
		AccountID:   "acct-api",
	})
	if err != nil {
		t.Fatalf("requestAccountUsageViaAPI: %v", err)
	}

	summary := formatUsageSummary(snapshots)
	if want := "Pro |  58% left in     3m | Credits 9.99"; summary != want {
		t.Fatalf("unexpected API usage summary:\n got: %s\nwant: %s", summary, want)
	}
}

func TestRequestAccountUsageReadsHeadersFromUsageLimitResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertUsageProbeRequest(t, r, "Bearer exhausted-token", "")
		for name, value := range map[string]string{
			"x-codex-primary-used-percent":     "100",
			"x-codex-primary-window-minutes":   "15",
			"x-codex-secondary-used-percent":   "87.5",
			"x-codex-secondary-window-minutes": "60",
		} {
			w.Header().Set(name, value)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSONResponse(t, w, map[string]any{
			"error": map[string]any{
				"type":    "usage_limit_reached",
				"message": "limit reached",
			},
		})
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	snapshots, err := requestAccountUsageViaChat(server.Client(), usageAuth{
		AccessToken: "exhausted-token",
		PlanType:    accountPlanTypePro,
	})
	if err != nil {
		t.Fatalf("requestAccountUsageViaChat: %v", err)
	}

	summary := formatUsageSummary(snapshots)
	if want := "Pro |   0% left / 15m | 12.5% left / 60m"; summary != want {
		t.Fatalf("unexpected exhausted usage summary:\n got: %s\nwant: %s", summary, want)
	}
}

func TestRequestAccountUsageViaChatIgnoresZeroOnlyHeaderWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertUsageProbeRequest(t, r, "Bearer zero-only-token", "")
		writeUsageProbeResponse(t, w, map[string]string{
			"x-codex-primary-used-percent": "0",
		})
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	snapshots, err := requestAccountUsageViaChat(server.Client(), usageAuth{
		AccessToken: "zero-only-token",
		PlanType:    accountPlanTypePro,
	})
	if err != nil {
		t.Fatalf("requestAccountUsageViaChat: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("expected primary snapshot only, got %d", len(snapshots))
	}
	if snapshots[0].Primary != nil {
		t.Fatalf("expected zero-only primary window to be ignored, got %#v", snapshots[0].Primary)
	}

	summary := formatUsageSummary(snapshots)
	if strings.Contains(summary, "100% left") {
		t.Fatalf("zero-only window should not report 100%% remaining, got %q", summary)
	}

	metrics := usageRemainingMetricsFromSnapshots(snapshots)
	if metrics.HasFiveHourRemaining || metrics.HasSevenDayRemaining {
		t.Fatalf("zero-only window should not provide default-selection metrics: %#v", metrics)
	}
}

func TestEnrichCandidatesWithUsageStartsRequestsInStaggeredParallel(t *testing.T) {
	dir := t.TempDir()

	writeJSONFile(t, filepath.Join(dir, "auth.json.a"), map[string]any{
		"tokens": map[string]any{
			"access_token": "first-token",
		},
	})
	writeJSONFile(t, filepath.Join(dir, "auth.json.b"), map[string]any{
		"tokens": map[string]any{
			"access_token": "second-token",
		},
	})

	var (
		mu         sync.Mutex
		startTimes = map[string]time.Time{}
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := strings.TrimSpace(r.Header.Get("Authorization"))

		mu.Lock()
		startTimes[authHeader] = time.Now()
		mu.Unlock()

		time.Sleep(200 * time.Millisecond)
		writeUsageProbeResponse(t, w, map[string]string{
			"x-codex-primary-used-percent":   "42",
			"x-codex-primary-window-minutes": "5",
			"x-codex-primary-reset-at":       "123",
		})
	}))
	t.Cleanup(server.Close)
	setUsageBaseURLForTest(t, server.URL+"/backend-api")

	candidates, err := loadCandidates(dir)
	if err != nil {
		t.Fatalf("loadCandidates: %v", err)
	}

	start := time.Now()
	enrichCandidatesWithUsage(candidates, usageModeChat)
	elapsed := time.Since(start)

	mu.Lock()
	firstStart, firstOK := startTimes["Bearer first-token"]
	secondStart, secondOK := startTimes["Bearer second-token"]
	mu.Unlock()

	if !firstOK || !secondOK {
		t.Fatalf("expected both requests to start, got %#v", startTimes)
	}

	gap := secondStart.Sub(firstStart)
	if gap < 40*time.Millisecond {
		t.Fatalf("expected at least 40ms stagger, got %v", gap)
	}
	if gap > 170*time.Millisecond {
		t.Fatalf("expected overlapping requests instead of serial starts, got %v", gap)
	}
	if elapsed > 360*time.Millisecond {
		t.Fatalf("expected staggered parallel runtime, got %v", elapsed)
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}

func assertUsageProbeRequest(t *testing.T, r *http.Request, expectedAuth, expectedAccount string) {
	t.Helper()

	if r.Method != http.MethodPost {
		t.Fatalf("unexpected method: %s", r.Method)
	}
	if r.URL.Path != "/backend-api/codex/responses" {
		t.Fatalf("unexpected path: %s", r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != expectedAuth {
		t.Fatalf("unexpected auth header: %s", got)
	}
	if got := r.Header.Get("ChatGPT-Account-ID"); got != expectedAccount {
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

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode usage probe request: %v", err)
	}
	if got := body["model"]; got != defaultUsageProbeModel {
		t.Fatalf("unexpected usage probe model: %#v", got)
	}
	if got := body["stream"]; got != true {
		t.Fatalf("usage probe should be streaming, got %#v", got)
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("expected reasoning object, got %#v", body["reasoning"])
	}
	if got := reasoning["effort"]; got != "none" {
		t.Fatalf("unexpected reasoning effort: %#v", got)
	}
}

func assertUsageAPIRequest(t *testing.T, r *http.Request, expectedAuth, expectedAccount string) {
	t.Helper()

	if r.Method != http.MethodGet {
		t.Fatalf("unexpected method: %s", r.Method)
	}
	if r.URL.Path != "/backend-api/wham/usage" {
		t.Fatalf("unexpected path: %s", r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != expectedAuth {
		t.Fatalf("unexpected auth header: %s", got)
	}
	if got := r.Header.Get("ChatGPT-Account-ID"); got != expectedAccount {
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
}

func writeUsageProbeResponse(t *testing.T, w http.ResponseWriter, headers map[string]string) {
	t.Helper()

	for name, value := range headers {
		w.Header().Set(name, value)
	}
	writeJSONResponse(t, w, map[string]any{
		"id": "resp_usage_probe",
		"usage": map[string]any{
			"input_tokens":  1,
			"output_tokens": 1,
			"total_tokens":  2,
		},
	})
}
