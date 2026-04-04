package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultOriginator               = "codex_cli_rs"
	codexInternalOriginatorEnv      = "CODEX_INTERNAL_ORIGINATOR_OVERRIDE"
	defaultRefreshTokenURL          = "https://auth.openai.com/oauth/token"
	refreshTokenURLOverrideEnv      = "CODEX_REFRESH_TOKEN_URL_OVERRIDE"
	codexCACertificateEnv           = "CODEX_CA_CERTIFICATE"
	sslCertFileEnv                  = "SSL_CERT_FILE"
	clientID                        = "app_EMoamEEZ73f0CkXaXp7hrann"
	refreshTokenExpiredMessage      = "Your access token could not be refreshed because your refresh token has expired. Please log out and sign in again."
	refreshTokenReusedMessage       = "Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again."
	refreshTokenInvalidatedMessage  = "Your access token could not be refreshed because your refresh token was revoked. Please log out and sign in again."
	refreshTokenUnknownMessage      = "Your access token could not be refreshed. Please log out and sign in again."
	defaultComputedUserAgentVersion = "0.0.0"
	defaultRequestTimeout           = 30 * time.Second
	defaultAuthFilePermission       = 0o600
	defaultRefreshMinAgeDays        = 7
)

type authPayload struct {
	AuthMode     *string    `json:"auth_mode,omitempty"`
	OpenAIAPIKey *string    `json:"OPENAI_API_KEY"`
	Tokens       *tokenData `json:"tokens,omitempty"`
	LastRefresh  *time.Time `json:"last_refresh,omitempty"`
}

type tokenData struct {
	IDToken      string  `json:"id_token"`
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	AccountID    *string `json:"account_id"`
}

type authDocument struct {
	raw    map[string]json.RawMessage
	parsed authPayload
}

type refreshRequest struct {
	ClientID     string `json:"client_id"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

type refreshResponse struct {
	IDToken      *string `json:"id_token"`
	AccessToken  *string `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
}

type refreshConfig struct {
	Originator      string
	UserAgent       string
	UserAgentSuffix string
	CodexVersion    string
	Residency       string
	Timeout         time.Duration
}

type refreshTokenFailedReason string

const (
	refreshTokenFailedReasonExpired   refreshTokenFailedReason = "expired"
	refreshTokenFailedReasonExhausted refreshTokenFailedReason = "exhausted"
	refreshTokenFailedReasonRevoked   refreshTokenFailedReason = "revoked"
	refreshTokenFailedReasonOther     refreshTokenFailedReason = "other"
)

type refreshTokenPermanentError struct {
	Reason  refreshTokenFailedReason
	Message string
}

func (e *refreshTokenPermanentError) Error() string {
	return e.Message
}

type refreshResultStatus string

const (
	refreshResultRefreshed refreshResultStatus = "refreshed"
	refreshResultSkipped   refreshResultStatus = "skipped"
	refreshResultFailed    refreshResultStatus = "failed"
)

type refreshResult struct {
	Status  refreshResultStatus
	Message string
}

func refreshAuthAliases(w io.Writer, codexDir string, candidates []candidate, minAgeDays int) error {
	if len(candidates) == 0 {
		return fmt.Errorf("no auth.json.* files found in %s", codexDir)
	}
	if minAgeDays < 0 {
		return fmt.Errorf("refresh days must be >= 0")
	}

	fmt.Fprintf(w, "Refreshing auth aliases in: %s\n", codexDir)

	results := make([]refreshResult, len(candidates))
	documents := make([]*authDocument, len(candidates))
	groups := make(map[string][]int)
	tokenOrder := make([]string, 0)
	groupNeedsRefresh := make(map[string]bool)
	skipReasons := make([]string, len(candidates))
	refreshedNow := nowFunc().UTC()

	for i, candidate := range candidates {
		doc, err := loadAuthDocument(candidate.Path)
		if err != nil {
			results[i] = refreshResult{
				Status:  refreshResultFailed,
				Message: err.Error(),
			}
			continue
		}

		mode := resolvedAuthMode(doc.parsed)
		if mode != "chatgpt" {
			results[i] = refreshResult{
				Status:  refreshResultSkipped,
				Message: fmt.Sprintf("auth_mode resolves to %q", mode),
			}
			continue
		}

		if doc.parsed.Tokens == nil {
			results[i] = refreshResult{
				Status:  refreshResultSkipped,
				Message: "auth.json does not contain tokens",
			}
			continue
		}

		refreshToken := strings.TrimSpace(doc.parsed.Tokens.RefreshToken)
		if refreshToken == "" {
			results[i] = refreshResult{
				Status:  refreshResultSkipped,
				Message: "auth.json does not contain a refresh_token",
			}
			continue
		}

		documents[i] = &doc
		shouldRefresh, skipReason := shouldRefreshAlias(doc.parsed.LastRefresh, refreshedNow, minAgeDays)
		skipReasons[i] = skipReason
		if _, ok := groups[refreshToken]; !ok {
			tokenOrder = append(tokenOrder, refreshToken)
		}
		groups[refreshToken] = append(groups[refreshToken], i)
		if shouldRefresh {
			groupNeedsRefresh[refreshToken] = true
		}
	}

	refreshTokens := make([]string, 0, len(tokenOrder))
	for _, token := range tokenOrder {
		if groupNeedsRefresh[token] {
			refreshTokens = append(refreshTokens, token)
			continue
		}
		for _, index := range groups[token] {
			results[index] = refreshResult{
				Status:  refreshResultSkipped,
				Message: skipReasons[index],
			}
		}
	}

	if len(refreshTokens) != 0 {
		cfg := defaultRefreshConfig()
		client, err := buildHTTPClient(cfg)
		if err != nil {
			for _, token := range refreshTokens {
				for _, index := range groups[token] {
					results[index] = refreshResult{
						Status:  refreshResultFailed,
						Message: fmt.Sprintf("build HTTP client: %v", err),
					}
				}
			}
		} else {
			for _, token := range refreshTokens {
				response, err := requestChatGPTTokenRefresh(client, cfg, token)
				if err != nil {
					for _, index := range groups[token] {
						results[index] = refreshResult{
							Status:  refreshResultFailed,
							Message: err.Error(),
						}
					}
					continue
				}

				for _, index := range groups[token] {
					doc := documents[index]
					if doc == nil {
						results[index] = refreshResult{
							Status:  refreshResultFailed,
							Message: "internal error: missing auth document",
						}
						continue
					}

					if err := doc.applyRefresh(response, refreshedNow); err != nil {
						results[index] = refreshResult{
							Status:  refreshResultFailed,
							Message: err.Error(),
						}
						continue
					}

					if err := saveAuthDocument(candidates[index].Path, *doc); err != nil {
						results[index] = refreshResult{
							Status:  refreshResultFailed,
							Message: err.Error(),
						}
						continue
					}

					results[index] = refreshResult{
						Status:  refreshResultRefreshed,
						Message: fmt.Sprintf("last_refresh=%s", formatRefreshAge(refreshedNow, nowFunc().UTC())),
					}
				}
			}
		}
	}

	refreshedCount := 0
	skippedCount := 0
	failedCount := 0

	for i, candidate := range candidates {
		result := results[i]
		if result.Status == "" {
			result = refreshResult{
				Status:  refreshResultSkipped,
				Message: "no refresh action taken",
			}
		}

		switch result.Status {
		case refreshResultRefreshed:
			refreshedCount++
		case refreshResultSkipped:
			skippedCount++
		case refreshResultFailed:
			failedCount++
		}

		if result.Message == "" {
			fmt.Fprintf(w, "[%s] %s\n", result.Status, candidate.Name)
			continue
		}
		fmt.Fprintf(w, "[%s] %s: %s\n", result.Status, candidate.Name, result.Message)
	}

	fmt.Fprintf(w, "Summary: %d refreshed, %d skipped, %d failed\n", refreshedCount, skippedCount, failedCount)
	if failedCount != 0 {
		return fmt.Errorf("refresh failed for %d auth file(s)", failedCount)
	}

	return nil
}

func shouldRefreshAlias(lastRefresh *time.Time, now time.Time, minAgeDays int) (bool, string) {
	if minAgeDays == 0 {
		return true, ""
	}
	if lastRefresh == nil || lastRefresh.IsZero() {
		return true, ""
	}

	last := lastRefresh.UTC()
	threshold := now.Add(-time.Duration(minAgeDays) * 24 * time.Hour)
	if last.After(threshold) {
		return false, fmt.Sprintf("last_refresh=%s is newer than %dd threshold", formatRefreshAge(last, now), minAgeDays)
	}

	return true, ""
}

func formatRefreshAge(value, now time.Time) string {
	delta := now.Sub(value)
	if delta < 0 {
		delta = -delta
	}
	return formatRelativeAmount(delta)
}

func loadAuthDocument(path string) (authDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return authDocument{}, fmt.Errorf("read %s: %w", path, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return authDocument{}, fmt.Errorf("parse %s: %w", path, err)
	}

	var parsed authPayload
	if err := json.Unmarshal(data, &parsed); err != nil {
		return authDocument{}, fmt.Errorf("decode %s: %w", path, err)
	}

	return authDocument{
		raw:    raw,
		parsed: parsed,
	}, nil
}

func resolvedAuthMode(auth authPayload) string {
	if auth.AuthMode != nil && strings.TrimSpace(*auth.AuthMode) != "" {
		return strings.TrimSpace(*auth.AuthMode)
	}
	if auth.OpenAIAPIKey != nil {
		return "apikey"
	}
	return "chatgpt"
}

func (doc *authDocument) applyRefresh(response refreshResponse, refreshedAt time.Time) error {
	if doc == nil {
		return errors.New("auth document is nil")
	}

	tokensRaw, ok := doc.raw["tokens"]
	if !ok {
		return errors.New("auth.json does not contain tokens")
	}

	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(tokensRaw, &tokens); err != nil {
		return fmt.Errorf("decode tokens object: %w", err)
	}

	if response.IDToken != nil {
		value, err := json.Marshal(*response.IDToken)
		if err != nil {
			return fmt.Errorf("encode id_token: %w", err)
		}
		tokens["id_token"] = value
	}
	if response.AccessToken != nil {
		value, err := json.Marshal(*response.AccessToken)
		if err != nil {
			return fmt.Errorf("encode access_token: %w", err)
		}
		tokens["access_token"] = value
	}
	if response.RefreshToken != nil {
		value, err := json.Marshal(*response.RefreshToken)
		if err != nil {
			return fmt.Errorf("encode refresh_token: %w", err)
		}
		tokens["refresh_token"] = value
	}

	encodedTokens, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("encode tokens object: %w", err)
	}
	doc.raw["tokens"] = encodedTokens

	encodedLastRefresh, err := json.Marshal(refreshedAt.UTC())
	if err != nil {
		return fmt.Errorf("encode last_refresh: %w", err)
	}
	doc.raw["last_refresh"] = encodedLastRefresh

	return nil
}

func saveAuthDocument(path string, doc authDocument) error {
	data, err := json.MarshalIndent(doc.raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	data = append(data, '\n')

	if err := writeFileAtomically(path, data, defaultAuthFilePermission); err != nil {
		return fmt.Errorf("persist %s: %w", path, err)
	}

	return nil
}

func writeFileAtomically(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tempFile, err := os.CreateTemp(filepath.Dir(path), ".auth.json.*.tmp")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if err := tempFile.Chmod(mode); err != nil && !errors.Is(err, os.ErrPermission) {
		_ = tempFile.Close()
		return err
	}
	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}

	if err := os.Rename(tempPath, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("replace %s: rename failed (%v), remove failed (%w)", path, err, removeErr)
		}
		if renameErr := os.Rename(tempPath, path); renameErr != nil {
			return fmt.Errorf("replace %s after remove: %w", path, renameErr)
		}
	}
	if err := os.Chmod(path, mode); err != nil && !errors.Is(err, os.ErrPermission) {
		return err
	}

	cleanup = false
	return nil
}

func defaultRefreshConfig() refreshConfig {
	return refreshConfig{
		CodexVersion: defaultComputedUserAgentVersion,
		Timeout:      defaultRequestTimeout,
	}
}

func buildHTTPClient(cfg refreshConfig) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if os.Getenv("CODEX_SANDBOX") == "seatbelt" {
		transport.Proxy = nil
	}

	rootCAs, err := configuredCustomCAPool()
	if err != nil {
		return nil, err
	}
	if rootCAs != nil {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		transport.TLSClientConfig.RootCAs = rootCAs
	}

	return &http.Client{
		Timeout:   cfg.Timeout,
		Transport: transport,
	}, nil
}

func configuredCustomCAPool() (*x509.CertPool, error) {
	path, sourceEnv := configuredCABundlePath()
	if path == "" {
		return nil, nil
	}

	pemData, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate file %s selected by %s: %w", path, sourceEnv, err)
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if ok := pool.AppendCertsFromPEM(pemData); !ok {
		return nil, fmt.Errorf("failed to load CA certificates from %s selected by %s: no parseable CERTIFICATE blocks found", path, sourceEnv)
	}

	return pool, nil
}

func configuredCABundlePath() (path string, sourceEnv string) {
	if value := strings.TrimSpace(os.Getenv(codexCACertificateEnv)); value != "" {
		return value, codexCACertificateEnv
	}
	if value := strings.TrimSpace(os.Getenv(sslCertFileEnv)); value != "" {
		return value, sslCertFileEnv
	}
	return "", ""
}

func requestChatGPTTokenRefresh(client *http.Client, cfg refreshConfig, refreshToken string) (refreshResponse, error) {
	payload := refreshRequest{
		ClientID:     clientID,
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return refreshResponse{}, err
	}

	req, err := http.NewRequest(http.MethodPost, refreshTokenEndpoint(), bytes.NewReader(body))
	if err != nil {
		return refreshResponse{}, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("originator", resolveOriginator(cfg.Originator))
	req.Header.Set("User-Agent", resolveUserAgent(cfg))

	resp, err := client.Do(req)
	if err != nil {
		return refreshResponse{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return refreshResponse{}, err
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var parsed refreshResponse
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return refreshResponse{}, err
		}
		return parsed, nil
	}

	bodyText := string(respBody)
	if resp.StatusCode == http.StatusUnauthorized {
		return refreshResponse{}, classifyRefreshTokenFailure(bodyText)
	}

	return refreshResponse{}, fmt.Errorf("failed to refresh token: %s: %s", resp.Status, tryParseErrorMessage(bodyText))
}

func refreshTokenEndpoint() string {
	if value := strings.TrimSpace(os.Getenv(refreshTokenURLOverrideEnv)); value != "" {
		return value
	}
	return defaultRefreshTokenURL
}

func classifyRefreshTokenFailure(body string) error {
	code := strings.ToLower(strings.TrimSpace(extractRefreshTokenErrorCode(body)))

	reason := refreshTokenFailedReasonOther
	message := refreshTokenUnknownMessage

	switch code {
	case "refresh_token_expired":
		reason = refreshTokenFailedReasonExpired
		message = refreshTokenExpiredMessage
	case "refresh_token_reused":
		reason = refreshTokenFailedReasonExhausted
		message = refreshTokenReusedMessage
	case "refresh_token_invalidated":
		reason = refreshTokenFailedReasonRevoked
		message = refreshTokenInvalidatedMessage
	}

	return &refreshTokenPermanentError{
		Reason:  reason,
		Message: message,
	}
}

func extractRefreshTokenErrorCode(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}

	var payload any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return ""
	}

	root, ok := payload.(map[string]any)
	if !ok {
		return ""
	}

	if rawError, ok := root["error"]; ok {
		switch value := rawError.(type) {
		case map[string]any:
			if code, ok := value["code"].(string); ok {
				return code
			}
		case string:
			return value
		}
	}

	code, _ := root["code"].(string)
	return code
}

func tryParseErrorMessage(text string) string {
	if strings.TrimSpace(text) == "" {
		return "Unknown error"
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return text
	}

	if rawError, ok := payload["error"].(map[string]any); ok {
		if message, ok := rawError["message"].(string); ok && message != "" {
			return message
		}
	}

	return text
}

func resolveOriginator(flagValue string) string {
	value := strings.TrimSpace(os.Getenv(codexInternalOriginatorEnv))
	if value == "" {
		value = strings.TrimSpace(flagValue)
	}
	if value == "" {
		value = defaultOriginator
	}
	if isValidHeaderValue(value) {
		return value
	}
	return defaultOriginator
}

func resolveUserAgent(cfg refreshConfig) string {
	if explicit := strings.TrimSpace(cfg.UserAgent); explicit != "" {
		if isValidHeaderValue(explicit) {
			return explicit
		}
		return sanitizeUserAgent(explicit, defaultOriginator)
	}

	version := cfg.CodexVersion
	if version == "" {
		version = defaultComputedUserAgentVersion
	}
	originator := resolveOriginator(cfg.Originator)
	osType, osVersion := detectOSInfo()
	terminalToken := detectTerminalUserAgentToken()

	prefix := fmt.Sprintf("%s/%s (%s %s; %s) %s", originator, version, osType, osVersion, runtime.GOARCH, terminalToken)
	candidate := prefix
	if suffix := strings.TrimSpace(cfg.UserAgentSuffix); suffix != "" {
		candidate = fmt.Sprintf("%s (%s)", prefix, suffix)
	}
	return sanitizeUserAgent(candidate, prefix)
}

func sanitizeUserAgent(candidate string, fallback string) string {
	if isValidHeaderValue(candidate) {
		return candidate
	}

	var b strings.Builder
	b.Grow(len(candidate))
	for _, r := range candidate {
		if r >= ' ' && r <= '~' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}

	sanitized := b.String()
	if sanitized != "" && isValidHeaderValue(sanitized) {
		return sanitized
	}
	if isValidHeaderValue(fallback) {
		return fallback
	}
	return defaultOriginator
}

func isValidHeaderValue(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch == '\r' || ch == '\n' || ch == 0 {
			return false
		}
	}
	return true
}

func detectOSInfo() (osType string, osVersion string) {
	switch runtime.GOOS {
	case "darwin":
		return "Macos", "unknown"
	case "windows":
		return "Windows", "unknown"
	case "linux":
		name, version := linuxOSRelease()
		if name != "" {
			return name, version
		}
		return "Linux", "unknown"
	default:
		return titleRuntimeOS(runtime.GOOS), "unknown"
	}
}

func titleRuntimeOS(name string) string {
	if name == "" {
		return "Unknown"
	}
	if len(name) == 1 {
		return strings.ToUpper(name)
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func linuxOSRelease() (string, string) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "", ""
	}

	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		values[key] = strings.Trim(value, `"`)
	}

	name := values["NAME"]
	if name == "" {
		name = values["ID"]
	}

	version := values["VERSION_ID"]
	if version == "" {
		version = "unknown"
	}

	return name, version
}

func detectTerminalUserAgentToken() string {
	termProgram := strings.TrimSpace(os.Getenv("TERM_PROGRAM"))
	termProgramVersion := strings.TrimSpace(os.Getenv("TERM_PROGRAM_VERSION"))
	term := strings.TrimSpace(os.Getenv("TERM"))

	switch {
	case termProgram != "" && termProgramVersion != "":
		return sanitizeUserAgent(fmt.Sprintf("%s/%s", termProgram, termProgramVersion), "unknown")
	case termProgram != "":
		return sanitizeUserAgent(termProgram, "unknown")
	case term != "":
		return sanitizeUserAgent(term, "unknown")
	default:
		return "unknown"
	}
}
