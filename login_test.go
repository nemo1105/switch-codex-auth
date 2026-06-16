package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunCLILoginCommandSavesAliasWithoutTouchingActiveAuth(t *testing.T) {
	fixedNow := time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		nowFunc = oldNow
	})

	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeAuthFile(t, filepath.Join(dir, "auth.json"), "active-auth")

	idToken := testJWT(t, map[string]any{
		"email": "login@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "workspace-123",
		},
	})
	var tokenRequests []url.Values
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("unexpected token path: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		tokenRequests = append(tokenRequests, r.Form)

		switch r.Form.Get("grant_type") {
		case "authorization_code":
			writeJSONResponse(t, w, map[string]any{
				"id_token":      idToken,
				"access_token":  "oauth-access",
				"refresh_token": "oauth-refresh",
			})
		case "urn:ietf:params:oauth:grant-type:token-exchange":
			writeJSONResponse(t, w, map[string]any{
				"access_token": "exchanged-api-key",
			})
		default:
			t.Fatalf("unexpected grant_type: %s", r.Form.Get("grant_type"))
		}
	}))
	t.Cleanup(tokenServer.Close)

	var openedURL string
	withLoginTestConfig(t, tokenServer.URL, func(rawURL string) error {
		openedURL = rawURL
		parsed, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse auth URL: %v", err)
		}
		if parsed.Path != "/oauth/authorize" {
			t.Fatalf("unexpected auth path: %s", parsed.Path)
		}
		if got := parsed.Query().Get("client_id"); got != clientID {
			t.Fatalf("unexpected client_id: %s", got)
		}
		if got := parsed.Query().Get("code_challenge"); got != "challenge-test" {
			t.Fatalf("unexpected code_challenge: %s", got)
		}
		if got := parsed.Query().Get("state"); got != "state-test" {
			t.Fatalf("unexpected state: %s", got)
		}

		callbackURL := parsed.Query().Get("redirect_uri") + "?code=code-test&state=state-test"
		resp, err := http.Get(callbackURL)
		if err != nil {
			t.Fatalf("callback request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected callback status: %s", resp.Status)
		}
		return nil
	})

	var out bytes.Buffer
	if err := runCLI([]string{"login", "demo"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	if openedURL == "" {
		t.Fatal("expected browser opener to receive auth URL")
	}
	if got := readAuthFile(t, filepath.Join(dir, "auth.json")); got != "active-auth" {
		t.Fatalf("active auth was modified: %q", got)
	}

	alias := readJSONFile(t, filepath.Join(dir, "auth.json.demo"))
	if got := alias["auth_mode"]; got != "chatgpt" {
		t.Fatalf("unexpected auth_mode: %#v", got)
	}
	if got := alias["OPENAI_API_KEY"]; got != "exchanged-api-key" {
		t.Fatalf("unexpected API key: %#v", got)
	}
	tokens := nestedMap(t, alias, "tokens")
	if got := tokens["id_token"]; got != idToken {
		t.Fatalf("unexpected id_token: %#v", got)
	}
	if got := tokens["access_token"]; got != "oauth-access" {
		t.Fatalf("unexpected access_token: %#v", got)
	}
	if got := tokens["refresh_token"]; got != "oauth-refresh" {
		t.Fatalf("unexpected refresh_token: %#v", got)
	}
	if got := tokens["account_id"]; got != "workspace-123" {
		t.Fatalf("unexpected account_id: %#v", got)
	}
	if got := alias["last_refresh"]; got != "2026-06-16T10:00:00Z" {
		t.Fatalf("unexpected last_refresh: %#v", got)
	}

	if len(tokenRequests) != 2 {
		t.Fatalf("expected two token requests, got %d", len(tokenRequests))
	}
	authCodeReq := tokenRequests[0]
	if got := authCodeReq.Get("code_verifier"); got != "verifier-test" {
		t.Fatalf("unexpected code_verifier: %s", got)
	}
	if got := authCodeReq.Get("code"); got != "code-test" {
		t.Fatalf("unexpected code: %s", got)
	}

	output := out.String()
	if !strings.Contains(output, "Open this URL to log in:") {
		t.Fatalf("expected login URL output, got:\n%s", output)
	}
	if !strings.Contains(output, "Saved login auth as: demo") {
		t.Fatalf("expected saved alias output, got:\n%s", output)
	}
	if !strings.Contains(output, filepath.Join(dir, "auth.json.demo")) {
		t.Fatalf("expected saved file path, got:\n%s", output)
	}
}

func TestLoginPromptsForAliasAndRetriesInvalidInput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)

	idToken := testJWT(t, map[string]any{"email": "login@example.com"})
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		writeJSONResponse(t, w, map[string]any{
			"id_token":      idToken,
			"access_token":  "oauth-access",
			"refresh_token": "oauth-refresh",
		})
	}))
	t.Cleanup(tokenServer.Close)

	withLoginTestConfig(t, tokenServer.URL, func(rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse auth URL: %v", err)
		}
		resp, err := http.Get(parsed.Query().Get("redirect_uri") + "?code=code-test&state=state-test")
		if err != nil {
			t.Fatalf("callback request: %v", err)
		}
		defer resp.Body.Close()
		return nil
	})

	var out bytes.Buffer
	input := strings.NewReader("bad/name\ncustom\n")
	if err := loginAndSaveAlias(dir, "", false, input, &out, true); err != nil {
		t.Fatalf("loginAndSaveAlias: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("active auth should not be created, stat err: %v", err)
	}
	if got := readJSONFile(t, filepath.Join(dir, "auth.json.custom")); nestedMap(t, got, "tokens")["access_token"] != "oauth-access" {
		t.Fatalf("unexpected saved auth: %#v", got)
	}

	output := out.String()
	if !strings.Contains(output, "Enter alias to save as: ") {
		t.Fatalf("expected alias prompt, got:\n%s", output)
	}
	if !strings.Contains(output, "Invalid alias: suffix cannot contain path separators: bad/name") {
		t.Fatalf("expected invalid alias message, got:\n%s", output)
	}
	if !strings.Contains(output, "Saved login auth as: custom") {
		t.Fatalf("expected custom save output, got:\n%s", output)
	}
}

func TestRunCLILoginWithoutSuffixNonInteractiveFailsBeforeOAuth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)

	var opened bool
	withLoginTestConfig(t, "https://example.test", func(rawURL string) error {
		opened = true
		return nil
	})

	var out bytes.Buffer
	err := runCLI([]string{"login"}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("expected missing suffix error")
	}
	if !strings.Contains(err.Error(), "login requires <suffix> when stdin is non-interactive") {
		t.Fatalf("unexpected error: %v", err)
	}
	if opened {
		t.Fatal("browser should not open before non-interactive suffix validation")
	}
}

func TestRunCLILoginExistingAliasNonInteractiveFailsBeforeOAuth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "old-demo")

	var opened bool
	withLoginTestConfig(t, "https://example.test", func(rawURL string) error {
		opened = true
		return nil
	})

	var out bytes.Buffer
	err := runCLI([]string{"login", "demo"}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "auth alias already exists: auth.json.demo") {
		t.Fatalf("unexpected error: %v", err)
	}
	if opened {
		t.Fatal("browser should not open before non-interactive conflict validation")
	}
	if got := readAuthFile(t, filepath.Join(dir, "auth.json.demo")); got != "old-demo" {
		t.Fatalf("existing alias should not change, got %q", got)
	}
}

func TestRunCLILoginForceOverwritesExistingAlias(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "old-demo")

	idToken := testJWT(t, map[string]any{"email": "login@example.com"})
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(t, w, map[string]any{
			"id_token":      idToken,
			"access_token":  "oauth-access",
			"refresh_token": "oauth-refresh",
		})
	}))
	t.Cleanup(tokenServer.Close)

	withLoginTestConfig(t, tokenServer.URL, func(rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse auth URL: %v", err)
		}
		resp, err := http.Get(parsed.Query().Get("redirect_uri") + "?code=code-test&state=state-test")
		if err != nil {
			t.Fatalf("callback request: %v", err)
		}
		defer resp.Body.Close()
		return nil
	})

	var out bytes.Buffer
	if err := runCLI([]string{"login", "demo", "--force"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	got := readJSONFile(t, filepath.Join(dir, "auth.json.demo"))
	if access := nestedMap(t, got, "tokens")["access_token"]; access != "oauth-access" {
		t.Fatalf("expected overwritten login auth, got %#v", got)
	}
	if output := out.String(); !strings.Contains(output, "Overwrote auth alias: demo") {
		t.Fatalf("expected overwrite output, got:\n%s", output)
	}
}

func TestRunCLIHelpLoginShowsUsage(t *testing.T) {
	var out bytes.Buffer
	if err := runCLI([]string{"help", "login"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	if got := out.String(); !strings.Contains(got, "login [suffix] [-f|--force]") {
		t.Fatalf("unexpected help output:\n%s", got)
	}
}

func withLoginTestConfig(t *testing.T, issuer string, opener func(string) error) {
	t.Helper()

	oldIssuer := loginIssuer
	oldPorts := append([]int(nil), loginPorts...)
	oldOpenBrowser := openBrowserFunc
	oldGeneratePKCE := generatePKCEFunc
	oldGenerateState := generateStateFunc

	loginIssuer = issuer
	loginPorts = []int{0}
	openBrowserFunc = opener
	generatePKCEFunc = func() (pkceCodes, error) {
		return pkceCodes{
			Verifier:  "verifier-test",
			Challenge: "challenge-test",
		}, nil
	}
	generateStateFunc = func() (string, error) {
		return "state-test", nil
	}

	t.Cleanup(func() {
		loginIssuer = oldIssuer
		loginPorts = oldPorts
		openBrowserFunc = oldOpenBrowser
		generatePKCEFunc = oldGeneratePKCE
		generateStateFunc = oldGenerateState
	})
}
