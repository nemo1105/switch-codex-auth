package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultUsageBaseURL        = "https://chatgpt.com/backend-api"
	defaultUsageRequestTimeout = 5 * time.Second
	defaultUsageProbeModel     = "gpt-5.4"
	usageRequestStagger        = 50 * time.Millisecond
	usagePercentWidth          = 4
	usageDurationWidth         = 6
	fiveHourUsageWindowMins    = int64(5 * 60)
	sevenDayUsageWindowMins    = int64(7 * 24 * 60)
)

var usageBaseURL = defaultUsageBaseURL

type usageMode string

const (
	usageModeNone usageMode = "none"
	usageModeAPI  usageMode = "api"
	usageModeChat usageMode = "chat"
)

type accountPlanType string

const (
	accountPlanTypeFree                    accountPlanType = "free"
	accountPlanTypeGo                      accountPlanType = "go"
	accountPlanTypePlus                    accountPlanType = "plus"
	accountPlanTypePro                     accountPlanType = "pro"
	accountPlanTypeTeam                    accountPlanType = "team"
	accountPlanTypeSelfServeBusiness       accountPlanType = "self_serve_business_usage_based"
	accountPlanTypeBusiness                accountPlanType = "business"
	accountPlanTypeEnterpriseCBPUsageBased accountPlanType = "enterprise_cbp_usage_based"
	accountPlanTypeEnterprise              accountPlanType = "enterprise"
	accountPlanTypeEdu                     accountPlanType = "edu"
	accountPlanTypeUnknown                 accountPlanType = "unknown"
)

type accountCreditsSnapshot struct {
	HasCredits bool
	Unlimited  bool
	Balance    string
}

type accountRateLimitWindow struct {
	UsedPercent        float64
	WindowDurationMins *int64
	ResetsAt           *int64
}

type accountRateLimitSnapshot struct {
	LimitID   string
	LimitName string
	Primary   *accountRateLimitWindow
	Secondary *accountRateLimitWindow
	Credits   *accountCreditsSnapshot
	PlanType  accountPlanType
}

type rateLimitStatusPayload struct {
	PlanType             string                       `json:"plan_type"`
	RateLimit            *rateLimitStatusDetails      `json:"rate_limit"`
	AdditionalRateLimits []additionalRateLimitDetails `json:"additional_rate_limits"`
	Credits              *creditStatusDetails         `json:"credits"`
}

type rateLimitStatusDetails struct {
	PrimaryWindow   *rateLimitWindowSnapshot `json:"primary_window"`
	SecondaryWindow *rateLimitWindowSnapshot `json:"secondary_window"`
}

type additionalRateLimitDetails struct {
	LimitName      string                  `json:"limit_name"`
	MeteredFeature string                  `json:"metered_feature"`
	RateLimit      *rateLimitStatusDetails `json:"rate_limit"`
}

type creditStatusDetails struct {
	HasCredits bool    `json:"has_credits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance"`
}

type rateLimitWindowSnapshot struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type usageErrorEnvelope struct {
	Error   *usageErrorDetails `json:"error"`
	Code    string             `json:"code"`
	Message string             `json:"message"`
	Status  int                `json:"status"`
}

type usageErrorDetails struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type usageAuth struct {
	AccessToken string
	AccountID   string
	PlanType    accountPlanType
}

func parseUsageMode(value string) (usageMode, error) {
	switch mode := usageMode(strings.ToLower(strings.TrimSpace(value))); mode {
	case "", usageModeNone:
		return usageModeNone, nil
	case usageModeAPI:
		return usageModeAPI, nil
	case usageModeChat:
		return usageModeChat, nil
	default:
		return usageModeNone, fmt.Errorf("usage must be one of: none, api, chat")
	}
}

func enrichCandidatesWithUsage(candidates []candidate, mode usageMode) []candidate {
	enriched := append([]candidate(nil), candidates...)
	if len(enriched) == 0 || mode == usageModeNone {
		return enriched
	}

	type usageRequestGroup struct {
		auth    usageAuth
		indexes []int
	}

	requestGroups := make(map[string]*usageRequestGroup)
	requestOrder := make([]string, 0)

	for i := range enriched {
		doc, err := loadAuthDocument(enriched[i].Path)
		if err != nil {
			enriched[i].Usage = formatUsageError(err)
			continue
		}

		if resolvedAuthMode(doc.parsed) != "chatgpt" {
			enriched[i].Usage = "n/a"
			continue
		}
		if doc.parsed.Tokens == nil {
			enriched[i].Usage = "n/a"
			continue
		}

		accessToken := strings.TrimSpace(doc.parsed.Tokens.AccessToken)
		if accessToken == "" {
			enriched[i].Usage = "n/a"
			continue
		}

		accountID := ""
		if doc.parsed.Tokens.AccountID != nil {
			accountID = strings.TrimSpace(*doc.parsed.Tokens.AccountID)
		}

		groupKey := accessToken + "\x00" + accountID
		group, ok := requestGroups[groupKey]
		if !ok {
			group = &usageRequestGroup{
				auth: usageAuth{
					AccessToken: accessToken,
					AccountID:   accountID,
					PlanType:    extractPlanTypeFromAuth(doc.parsed),
				},
			}
			requestGroups[groupKey] = group
			requestOrder = append(requestOrder, groupKey)
		}
		group.indexes = append(group.indexes, i)
	}

	if len(requestOrder) == 0 {
		return enriched
	}

	client, err := buildHTTPClient(refreshConfig{
		CodexVersion: resolveClientVersion(),
		Timeout:      defaultUsageRequestTimeout,
	})
	if err != nil {
		for _, key := range requestOrder {
			for _, index := range requestGroups[key].indexes {
				enriched[index].Usage = formatUsageError(err)
			}
		}
		return enriched
	}

	type usageRequestResult struct {
		key             string
		summary         string
		remainingMetric usageRemainingMetrics
	}

	results := make(chan usageRequestResult, len(requestOrder))
	var wg sync.WaitGroup

	for i, key := range requestOrder {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()

			if delay := time.Duration(i) * usageRequestStagger; delay > 0 {
				timer := time.NewTimer(delay)
				defer timer.Stop()
				<-timer.C
			}

			snapshots, err := requestAccountUsage(client, requestGroups[key].auth, mode)
			summary := formatUsageError(err)
			var remainingMetric usageRemainingMetrics
			if err == nil {
				summary = formatUsageSummary(snapshots)
				remainingMetric = usageRemainingMetricsFromSnapshots(snapshots)
			}
			results <- usageRequestResult{
				key:             key,
				summary:         summary,
				remainingMetric: remainingMetric,
			}
		}(i, key)
	}

	wg.Wait()
	close(results)

	for result := range results {
		for _, index := range requestGroups[result.key].indexes {
			enriched[index].Usage = result.summary
			enriched[index].UsageRemaining = result.remainingMetric
		}
	}

	return enriched
}

func requestAccountUsage(client *http.Client, auth usageAuth, mode usageMode) ([]accountRateLimitSnapshot, error) {
	switch mode {
	case usageModeAPI:
		return requestAccountUsageViaAPI(client, auth)
	case usageModeChat:
		return requestAccountUsageViaChat(client, auth)
	case usageModeNone:
		return nil, fmt.Errorf("usage mode is none")
	default:
		return nil, fmt.Errorf("unknown usage mode: %s", mode)
	}
}

func requestAccountUsageViaAPI(client *http.Client, auth usageAuth) ([]accountRateLimitSnapshot, error) {
	url := usageAPIEndpointURL(normalizeUsageBaseURL(usageBaseURL))
	if url == "" {
		return nil, fmt.Errorf("usage base URL is empty")
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create usage request: %w", err)
	}

	req.Header = buildUsageRequestHeaders(auth)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perform usage request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read usage response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", summarizeUsageHTTPError(resp.StatusCode, body))
	}

	snapshots, err := decodeAccountUsagePayload(body)
	if err != nil {
		return nil, fmt.Errorf("decode usage response: %w", err)
	}
	return snapshots, nil
}

func requestAccountUsageViaChat(client *http.Client, auth usageAuth) ([]accountRateLimitSnapshot, error) {
	url := usageProbeEndpointURL(normalizeUsageBaseURL(usageBaseURL))
	if url == "" {
		return nil, fmt.Errorf("usage base URL is empty")
	}

	requestBody, err := json.Marshal(buildUsageProbeRequest())
	if err != nil {
		return nil, fmt.Errorf("encode usage probe request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("create usage request: %w", err)
	}

	req.Header = buildUsageRequestHeaders(auth)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perform usage request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read usage response: %w", err)
	}

	snapshots := accountRateLimitSnapshotsFromHeaders(resp.Header, auth.PlanType)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests && snapshotsHaveRateLimitData(snapshots) {
			return snapshots, nil
		}
		return nil, fmt.Errorf("%s", summarizeUsageHTTPError(resp.StatusCode, body))
	}

	if err := decodeUsageProbeResponse(body); err != nil {
		return nil, fmt.Errorf("decode usage response: %w", err)
	}
	return snapshots, nil
}

func buildUsageProbeRequest() map[string]any {
	return map[string]any{
		"model": defaultUsageProbeModel,
		"instructions": strings.Join([]string{
			"Return exactly OK.",
			"Do not include any other text.",
		}, " "),
		"input": []map[string]any{
			{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{
						"type": "input_text",
						"text": "OK",
					},
				},
			},
		},
		"tools":               []any{},
		"tool_choice":         "none",
		"parallel_tool_calls": false,
		"reasoning": map[string]string{
			"effort": "none",
		},
		"store":   false,
		"stream":  true,
		"include": []string{},
		"text": map[string]string{
			"verbosity": "low",
		},
	}
}

func decodeUsageProbeResponse(body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:")) {
		return nil
	}

	var payload struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}
	return nil
}

func accountRateLimitSnapshotsFromHeaders(headers http.Header, planType accountPlanType) []accountRateLimitSnapshot {
	snapshots := []accountRateLimitSnapshot{
		{
			LimitID:   "codex",
			Primary:   mapRateLimitHeaderWindow(headers, "x-codex-primary"),
			Secondary: mapRateLimitHeaderWindow(headers, "x-codex-secondary"),
			Credits:   mapCreditsHeaders(headers),
			PlanType:  planType,
		},
	}

	for _, limitID := range additionalLimitIDsFromHeaders(headers) {
		snapshot := accountRateLimitSnapshot{
			LimitID:   limitID,
			LimitName: strings.TrimSpace(headerValue(headers, "x-"+strings.ReplaceAll(limitID, "_", "-")+"-limit-name")),
			Primary:   mapRateLimitHeaderWindow(headers, "x-"+strings.ReplaceAll(limitID, "_", "-")+"-primary"),
			Secondary: mapRateLimitHeaderWindow(headers, "x-"+strings.ReplaceAll(limitID, "_", "-")+"-secondary"),
			PlanType:  planType,
		}
		if hasAccountRateLimitData(snapshot) {
			snapshots = append(snapshots, snapshot)
		}
	}

	return snapshots
}

func snapshotsHaveRateLimitData(snapshots []accountRateLimitSnapshot) bool {
	for _, snapshot := range snapshots {
		if hasAccountRateLimitData(snapshot) {
			return true
		}
	}
	return false
}

func mapRateLimitHeaderWindow(headers http.Header, prefix string) *accountRateLimitWindow {
	usedPercent, ok := headerFloat(headers, prefix+"-used-percent")
	if !ok {
		return nil
	}

	windowDurationMins, _ := headerInt64Ptr(headers, prefix+"-window-minutes")
	resetsAt, _ := headerInt64Ptr(headers, prefix+"-reset-at")
	hasData := usedPercent != 0 ||
		(windowDurationMins != nil && *windowDurationMins != 0) ||
		resetsAt != nil
	if !hasData {
		return nil
	}
	return &accountRateLimitWindow{
		UsedPercent:        usedPercent,
		WindowDurationMins: windowDurationMins,
		ResetsAt:           resetsAt,
	}
}

func mapCreditsHeaders(headers http.Header) *accountCreditsSnapshot {
	hasCredits, ok := headerBool(headers, "x-codex-credits-has-credits")
	if !ok {
		return nil
	}
	unlimited, ok := headerBool(headers, "x-codex-credits-unlimited")
	if !ok {
		return nil
	}
	return &accountCreditsSnapshot{
		HasCredits: hasCredits,
		Unlimited:  unlimited,
		Balance:    strings.TrimSpace(headerValue(headers, "x-codex-credits-balance")),
	}
}

func additionalLimitIDsFromHeaders(headers http.Header) []string {
	seen := make(map[string]bool)
	for name := range headers {
		normalized := strings.ToLower(name)
		if !strings.HasPrefix(normalized, "x-codex-") || !strings.HasSuffix(normalized, "-primary-used-percent") {
			continue
		}
		rawLimit := strings.TrimSuffix(strings.TrimPrefix(normalized, "x-"), "-primary-used-percent")
		limitID := normalizeLimitID(rawLimit)
		if limitID == "" || limitID == "codex" || seen[limitID] {
			continue
		}
		seen[limitID] = true
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func headerFloat(headers http.Header, name string) (float64, bool) {
	value := strings.TrimSpace(headerValue(headers, name))
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

func headerInt64Ptr(headers http.Header, name string) (*int64, bool) {
	value := strings.TrimSpace(headerValue(headers, name))
	if value == "" {
		return nil, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil, false
	}
	return &parsed, true
}

func headerBool(headers http.Header, name string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(headerValue(headers, name))) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

func headerValue(headers http.Header, name string) string {
	return headers.Get(name)
}

func extractPlanTypeFromAuth(auth authPayload) accountPlanType {
	if auth.Tokens == nil {
		return ""
	}
	idToken := strings.TrimSpace(auth.Tokens.IDToken)
	if idToken == "" {
		return ""
	}

	planType, err := extractPlanTypeFromJWT(idToken)
	if err != nil {
		return ""
	}
	return normalizePlanType(planType)
}

func extractPlanTypeFromJWT(jwt string) (string, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid JWT format")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}

	var claims idTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return "", err
	}
	if planType := strings.TrimSpace(claims.ChatGPTPlanType); planType != "" {
		return planType, nil
	}
	if claims.Auth != nil {
		if planType := strings.TrimSpace(claims.Auth.ChatGPTPlanType); planType != "" {
			return planType, nil
		}
	}
	return "", fmt.Errorf("plan type not found in JWT claims")
}

func buildUsageRequestHeaders(auth usageAuth) http.Header {
	headers := make(http.Header)
	headers.Set("originator", resolveOriginator(""))
	headers.Set("User-Agent", resolveUserAgent(refreshConfig{
		CodexVersion: resolveClientVersion(),
	}))
	headers.Set("version", resolveClientVersion())
	headers.Set("Authorization", "Bearer "+auth.AccessToken)
	if auth.AccountID != "" {
		headers.Set("ChatGPT-Account-ID", auth.AccountID)
	}
	return headers
}

func resolveClientVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if version := strings.TrimSpace(info.Main.Version); version != "" && version != "(devel)" {
			return version
		}
	}
	return defaultComputedUserAgentVersion
}

func normalizeUsageBaseURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	if strings.HasSuffix(baseURL, "/responses") {
		baseURL = strings.TrimSuffix(baseURL, "/responses")
	}
	if (strings.HasPrefix(baseURL, "https://chatgpt.com") || strings.HasPrefix(baseURL, "https://chat.openai.com")) &&
		!strings.Contains(baseURL, "/backend-api") {
		baseURL += "/backend-api"
	}
	return baseURL
}

func usageProbeEndpointURL(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	if strings.HasSuffix(baseURL, "/codex") || strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/responses"
	}
	return baseURL + "/codex/responses"
}

func usageAPIEndpointURL(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	if strings.HasSuffix(baseURL, "/codex") {
		baseURL = strings.TrimSuffix(baseURL, "/codex")
	}
	return baseURL + "/wham/usage"
}

func decodeAccountUsagePayload(data []byte) ([]accountRateLimitSnapshot, error) {
	var payload rateLimitStatusPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	return accountRateLimitSnapshotsFromPayload(payload), nil
}

func accountRateLimitSnapshotsFromPayload(payload rateLimitStatusPayload) []accountRateLimitSnapshot {
	planType := normalizePlanType(payload.PlanType)
	snapshots := []accountRateLimitSnapshot{
		makeAccountRateLimitSnapshot(
			"codex",
			"",
			payload.RateLimit,
			payload.Credits,
			planType,
		),
	}
	for _, details := range payload.AdditionalRateLimits {
		snapshots = append(snapshots, makeAccountRateLimitSnapshot(
			normalizeLimitID(details.MeteredFeature),
			strings.TrimSpace(details.LimitName),
			details.RateLimit,
			nil,
			planType,
		))
	}
	return snapshots
}

func makeAccountRateLimitSnapshot(
	limitID string,
	limitName string,
	rateLimit *rateLimitStatusDetails,
	credits *creditStatusDetails,
	planType accountPlanType,
) accountRateLimitSnapshot {
	var primary *accountRateLimitWindow
	var secondary *accountRateLimitWindow
	if rateLimit != nil {
		primary = mapRateLimitWindow(rateLimit.PrimaryWindow)
		secondary = mapRateLimitWindow(rateLimit.SecondaryWindow)
	}

	return accountRateLimitSnapshot{
		LimitID:   normalizeLimitID(limitID),
		LimitName: limitName,
		Primary:   primary,
		Secondary: secondary,
		Credits:   mapCreditsSnapshot(credits),
		PlanType:  planType,
	}
}

func mapRateLimitWindow(window *rateLimitWindowSnapshot) *accountRateLimitWindow {
	if window == nil {
		return nil
	}
	windowDurationMins := windowMinutesFromSeconds(window.LimitWindowSeconds)
	var resetsAt *int64
	if window.ResetAt > 0 {
		value := window.ResetAt
		resetsAt = &value
	}
	return &accountRateLimitWindow{
		UsedPercent:        window.UsedPercent,
		WindowDurationMins: windowDurationMins,
		ResetsAt:           resetsAt,
	}
}

func mapCreditsSnapshot(credits *creditStatusDetails) *accountCreditsSnapshot {
	if credits == nil {
		return nil
	}
	balance := ""
	if credits.Balance != nil {
		balance = strings.TrimSpace(*credits.Balance)
	}
	return &accountCreditsSnapshot{
		HasCredits: credits.HasCredits,
		Unlimited:  credits.Unlimited,
		Balance:    balance,
	}
}

func normalizeLimitID(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return ""
	}
	return strings.ReplaceAll(name, "-", "_")
}

func normalizePlanType(planType string) accountPlanType {
	normalized := strings.TrimSpace(strings.ToLower(planType))
	normalized = strings.NewReplacer("-", "_", " ", "_").Replace(normalized)
	switch normalized {
	case "":
		return ""
	case string(accountPlanTypeFree):
		return accountPlanTypeFree
	case string(accountPlanTypeGo):
		return accountPlanTypeGo
	case string(accountPlanTypePlus):
		return accountPlanTypePlus
	case string(accountPlanTypePro):
		return accountPlanTypePro
	case string(accountPlanTypeTeam):
		return accountPlanTypeTeam
	case "prolite", "pro_lite":
		return accountPlanTypePro
	case string(accountPlanTypeSelfServeBusiness):
		return accountPlanTypeSelfServeBusiness
	case string(accountPlanTypeBusiness):
		return accountPlanTypeBusiness
	case string(accountPlanTypeEnterpriseCBPUsageBased):
		return accountPlanTypeEnterpriseCBPUsageBased
	case string(accountPlanTypeEnterprise):
		return accountPlanTypeEnterprise
	case string(accountPlanTypeEdu), "education":
		return accountPlanTypeEdu
	case "selfservebusinessusagebased":
		return accountPlanTypeSelfServeBusiness
	case "enterprisecbpusagebased":
		return accountPlanTypeEnterpriseCBPUsageBased
	case "hc":
		return accountPlanTypeEnterprise
	default:
		return accountPlanTypeUnknown
	}
}

func windowMinutesFromSeconds(seconds int64) *int64 {
	if seconds <= 0 {
		return nil
	}
	minutes := (seconds + 59) / 60
	return &minutes
}

func formatUsageSummary(snapshots []accountRateLimitSnapshot) string {
	if len(snapshots) == 0 {
		return "n/a"
	}

	primary := snapshots[0]
	extraCount := countAdditionalRateLimits(snapshots)
	if !hasAccountRateLimitData(primary) && extraCount == 0 && strings.TrimSpace(string(primary.PlanType)) == "" {
		return "n/a"
	}

	parts := make([]string, 0, 6)

	if plan := formatUsagePlan(primary.PlanType); plan != "" {
		parts = append(parts, plan)
	}

	if primary.Primary != nil {
		parts = append(parts, formatUsageRateLimitWindow(*primary.Primary))
	}
	if primary.Secondary != nil {
		parts = append(parts, formatUsageRateLimitWindow(*primary.Secondary))
	}
	if credits := formatUsageCreditsSnapshot(primary.Credits); credits != "" {
		parts = append(parts, credits)
	}
	if extraCount > 0 {
		parts = append(parts, fmt.Sprintf("+%d extra", extraCount))
	}

	if len(parts) == 0 {
		return "n/a"
	}
	return strings.Join(parts, " | ")
}

func usageRemainingMetricsFromSnapshots(snapshots []accountRateLimitSnapshot) usageRemainingMetrics {
	var metrics usageRemainingMetrics
	if len(snapshots) == 0 {
		return metrics
	}

	primary := snapshots[0]
	if usageWindowMatchesDefaultDuration(primary.Primary, fiveHourUsageWindowMins) {
		metrics.FiveHourRemaining = remainingUsagePercent(primary.Primary.UsedPercent)
		metrics.HasFiveHourRemaining = true
	}
	if usageWindowMatchesDefaultDuration(primary.Secondary, sevenDayUsageWindowMins) {
		metrics.SevenDayRemaining = remainingUsagePercent(primary.Secondary.UsedPercent)
		metrics.HasSevenDayRemaining = true
	}
	return metrics
}

func usageWindowMatchesDefaultDuration(window *accountRateLimitWindow, minutes int64) bool {
	if window == nil {
		return false
	}
	return window.WindowDurationMins == nil || *window.WindowDurationMins == minutes
}

func countAdditionalRateLimits(snapshots []accountRateLimitSnapshot) int {
	if len(snapshots) <= 1 {
		return 0
	}

	extraCount := 0
	for _, snapshot := range snapshots[1:] {
		if hasAccountRateLimitData(snapshot) {
			extraCount++
		}
	}
	return extraCount
}

func hasAccountRateLimitData(snapshot accountRateLimitSnapshot) bool {
	return snapshot.Primary != nil || snapshot.Secondary != nil || snapshot.Credits != nil
}

func formatUsageRateLimitWindow(window accountRateLimitWindow) string {
	result := fmt.Sprintf(
		"%*s left",
		usagePercentWidth,
		formatUsedPercent(remainingUsagePercent(window.UsedPercent)),
	)
	if window.ResetsAt != nil {
		result += fmt.Sprintf(
			" in %*s",
			usageDurationWidth,
			formatUsageRelativeAmount(time.Unix(*window.ResetsAt, 0).Sub(nowFunc())),
		)
		return result
	}
	if window.WindowDurationMins != nil {
		result += fmt.Sprintf(" / %dm", *window.WindowDurationMins)
	}
	return result
}

func formatUsageCreditsSnapshot(snapshot *accountCreditsSnapshot) string {
	if snapshot == nil {
		return ""
	}
	switch {
	case snapshot.Unlimited:
		return "Credits unlimited"
	case strings.TrimSpace(snapshot.Balance) != "":
		return "Credits " + strings.TrimSpace(snapshot.Balance)
	default:
		return ""
	}
}

func formatUsagePlan(plan accountPlanType) string {
	switch plan {
	case accountPlanTypeFree:
		return "Free"
	case accountPlanTypeGo:
		return "Go"
	case accountPlanTypePlus:
		return "Plus"
	case accountPlanTypePro:
		return "Pro"
	case accountPlanTypeTeam:
		return "Team"
	case accountPlanTypeSelfServeBusiness:
		return "Self-serve Business"
	case accountPlanTypeBusiness:
		return "Business"
	case accountPlanTypeEnterpriseCBPUsageBased, accountPlanTypeEnterprise:
		return "Enterprise"
	case accountPlanTypeEdu:
		return "Edu"
	case accountPlanTypeUnknown:
		return ""
	}

	raw := strings.TrimSpace(string(plan))
	if raw == "" {
		return ""
	}

	raw = strings.ReplaceAll(raw, "_", " ")
	raw = strings.ReplaceAll(raw, "-", " ")
	parts := strings.Fields(raw)
	for i, part := range parts {
		if len(part) == 0 {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func formatUsageError(err error) string {
	if err == nil {
		return ""
	}
	if isUsageTimeoutError(err) {
		return "Request timeout"
	}
	return strings.Join(strings.Fields(strings.TrimSpace(err.Error())), " ")
}

func isUsageTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func summarizeUsageHTTPError(statusCode int, body []byte) string {
	statusLabel := strconv.Itoa(statusCode)

	var envelope usageErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil {
		if envelope.Status > 0 {
			statusLabel = strconv.Itoa(envelope.Status)
		}

		message := strings.TrimSpace(envelope.Message)
		code := strings.TrimSpace(envelope.Code)
		if envelope.Error != nil {
			if nestedMessage := strings.TrimSpace(envelope.Error.Message); nestedMessage != "" {
				message = nestedMessage
			}
			if nestedCode := strings.TrimSpace(envelope.Error.Code); nestedCode != "" {
				code = nestedCode
			}
		}

		switch {
		case message != "":
			return statusLabel + ": " + message
		case code != "":
			return statusLabel + ": " + code
		}
	}

	detail := strings.TrimSpace(string(body))
	if detail == "" {
		return statusLabel
	}
	return statusLabel + ": " + detail
}

func remainingUsagePercent(usedPercent float64) float64 {
	remaining := 100 - usedPercent
	switch {
	case remaining < 0:
		return 0
	case remaining > 100:
		return 100
	default:
		return remaining
	}
}

func formatUsageRelativeAmount(delta time.Duration) string {
	if delta <= 0 {
		return "<1m"
	}

	minutes := int64(delta / time.Minute)
	if minutes <= 0 {
		return "<1m"
	}

	days := minutes / (24 * 60)
	minutes -= days * 24 * 60
	hours := minutes / 60
	minutes -= hours * 60

	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

func formatUsedPercent(value float64) string {
	if math.Abs(value-math.Round(value)) < 0.00001 {
		return fmt.Sprintf("%.0f%%", value)
	}
	return fmt.Sprintf("%.1f%%", value)
}
