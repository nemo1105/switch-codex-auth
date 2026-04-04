package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const (
	defaultUsageBaseURL        = "https://chatgpt.com/backend-api"
	defaultUsageRequestTimeout = 5 * time.Second
	usagePercentWidth          = 4
	usageDurationWidth         = 6
)

var usageBaseURL = defaultUsageBaseURL

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
}

func enrichCandidatesWithUsage(candidates []candidate) []candidate {
	enriched := append([]candidate(nil), candidates...)
	if len(enriched) == 0 {
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

	for _, key := range requestOrder {
		snapshots, err := requestAccountUsage(client, requestGroups[key].auth)
		summary := formatUsageError(err)
		if err == nil {
			summary = formatUsageSummary(snapshots)
		}
		for _, index := range requestGroups[key].indexes {
			enriched[index].Usage = summary
		}
	}

	return enriched
}

func requestAccountUsage(client *http.Client, auth usageAuth) ([]accountRateLimitSnapshot, error) {
	url := usageEndpointURL(normalizeUsageBaseURL(usageBaseURL))
	if url == "" {
		return nil, fmt.Errorf("usage base URL is empty")
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create usage request: %w", err)
	}

	req.Header = buildUsageRequestHeaders(auth)
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
	if strings.HasSuffix(baseURL, "/codex") {
		baseURL = strings.TrimSuffix(baseURL, "/codex")
	}
	if (strings.HasPrefix(baseURL, "https://chatgpt.com") || strings.HasPrefix(baseURL, "https://chat.openai.com")) &&
		!strings.Contains(baseURL, "/backend-api") {
		baseURL += "/backend-api"
	}
	return baseURL
}

func usageEndpointURL(baseURL string) string {
	if baseURL == "" {
		return ""
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
