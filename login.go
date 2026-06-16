package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	defaultLoginIssuer = "https://auth.openai.com"
	defaultLoginPort   = 1455
	fallbackLoginPort  = 1457
)

var (
	loginIssuer       = defaultLoginIssuer
	loginPorts        = []int{defaultLoginPort, fallbackLoginPort}
	openBrowserFunc   = openBrowser
	generatePKCEFunc  = generatePKCE
	generateStateFunc = func() (string, error) {
		return randomBase64URL(32)
	}
)

type pkceCodes struct {
	Verifier  string
	Challenge string
}

type loginTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type apiKeyExchangeResponse struct {
	AccessToken string `json:"access_token"`
}

func loginAndSaveAlias(codexDir, selection string, force bool, in io.Reader, out io.Writer, interactive bool) error {
	reader := (*bufio.Reader)(nil)
	if interactive {
		reader = bufio.NewReader(in)
	}

	initialSuffix := strings.TrimSpace(selection)
	if initialSuffix != "" {
		suffix, err := normalizeSuffix(initialSuffix)
		if err != nil {
			return err
		}
		if reader == nil && !force {
			targetPath := filepath.Join(codexDir, "auth.json."+suffix)
			if _, err := os.Stat(targetPath); err == nil {
				return fmt.Errorf("auth alias already exists: %s (use --force to overwrite or choose a different alias)", filepath.Base(targetPath))
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("stat %s: %w", targetPath, err)
			}
		}
		initialSuffix = suffix
	} else if reader == nil {
		return errors.New("login requires <suffix> when stdin is non-interactive")
	}

	auth, err := runOAuthLogin(out)
	if err != nil {
		return err
	}

	suffix := initialSuffix
	if suffix == "" {
		var err error
		suffix, err = promptForInitialLoginAlias(reader, out)
		if err != nil {
			return err
		}
	}

	tempPath, err := writeTemporaryLoginAuth(codexDir, auth)
	if err != nil {
		return err
	}
	defer os.Remove(tempPath)

	finalSuffix, err := saveSourceAsAliasWithReader(codexDir, tempPath, suffix, force, reader, out, "Saved login auth as")
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Saved login auth file: %s\n", filepath.Join(codexDir, "auth.json."+finalSuffix))
	return nil
}

func promptForInitialLoginAlias(reader *bufio.Reader, out io.Writer) (string, error) {
	for {
		fmt.Fprint(out, "Enter alias to save as: ")
		selection, err := readPromptLine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", errors.New("login cancelled before alias was provided")
			}
			return "", fmt.Errorf("read login alias: %w", err)
		}

		suffix, err := normalizeSuffix(selection)
		if err != nil {
			fmt.Fprintf(out, "Invalid alias: %v\n", err)
			continue
		}
		return suffix, nil
	}
}

func runOAuthLogin(out io.Writer) (authPayload, error) {
	pkce, err := generatePKCEFunc()
	if err != nil {
		return authPayload{}, err
	}
	state, err := generateStateFunc()
	if err != nil {
		return authPayload{}, err
	}

	listener, port, err := listenForLoginCallback()
	if err != nil {
		return authPayload{}, err
	}
	defer listener.Close()

	resultCh := make(chan oauthLoginResult, 1)
	server := &http.Server{
		Handler: loginCallbackHandler(pkce, state, port, resultCh),
	}
	defer server.Shutdown(context.Background())

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			resultCh <- oauthLoginResult{err: err}
		}
	}()

	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)
	authURL := buildLoginAuthorizeURL(loginIssuer, redirectURI, pkce.Challenge, state)
	fmt.Fprintf(out, "Open this URL to log in:\n\n%s\n\n", authURL)
	if err := openBrowserFunc(authURL); err != nil {
		fmt.Fprintf(out, "Warning: failed to open browser: %v\n", err)
	}

	result := <-resultCh
	if result.err != nil {
		return authPayload{}, result.err
	}
	return result.auth, nil
}

type oauthLoginResult struct {
	auth authPayload
	err  error
}

func listenForLoginCallback() (net.Listener, int, error) {
	var lastErr error
	for _, port := range loginPorts {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			actualPort := port
			if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
				actualPort = tcpAddr.Port
			}
			return listener, actualPort, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("listen for login callback: %w", lastErr)
}

func loginCallbackHandler(pkce pkceCodes, state string, port int, resultCh chan<- oauthLoginResult) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "State mismatch", http.StatusBadRequest)
			resultCh <- oauthLoginResult{err: errors.New("login callback state mismatch")}
			return
		}
		if errorCode := r.URL.Query().Get("error"); errorCode != "" {
			message := fmt.Sprintf("sign-in failed: %s", errorCode)
			if description := strings.TrimSpace(r.URL.Query().Get("error_description")); description != "" {
				message = fmt.Sprintf("sign-in failed: %s", description)
			}
			http.Error(w, message, http.StatusForbidden)
			resultCh <- oauthLoginResult{err: errors.New(message)}
			return
		}

		code := strings.TrimSpace(r.URL.Query().Get("code"))
		if code == "" {
			http.Error(w, "Missing authorization code", http.StatusBadRequest)
			resultCh <- oauthLoginResult{err: errors.New("missing authorization code")}
			return
		}

		redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)
		auth, err := exchangeLoginCodeForAuth(pkce.Verifier, code, redirectURI)
		if err != nil {
			http.Error(w, "Token exchange failed", http.StatusBadGateway)
			resultCh <- oauthLoginResult{err: err}
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>Codex login complete</title><p>Codex login complete. You can return to the terminal.</p>")
		resultCh <- oauthLoginResult{auth: auth}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return mux
}

func exchangeLoginCodeForAuth(codeVerifier, code, redirectURI string) (authPayload, error) {
	cfg := defaultRefreshConfig()
	client, err := buildHTTPClient(cfg)
	if err != nil {
		return authPayload{}, fmt.Errorf("build HTTP client: %w", err)
	}

	tokenEndpoint := strings.TrimRight(loginIssuer, "/") + "/oauth/token"
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("code_verifier", codeVerifier)

	req, err := http.NewRequest(http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return authPayload{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("originator", resolveOriginator(cfg.Originator))
	req.Header.Set("User-Agent", resolveUserAgent(cfg))

	resp, err := client.Do(req)
	if err != nil {
		return authPayload{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return authPayload{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return authPayload{}, fmt.Errorf("token exchange failed: %s: %s", resp.Status, tryParseErrorMessage(string(body)))
	}

	var tokens loginTokens
	if err := json.Unmarshal(body, &tokens); err != nil {
		return authPayload{}, err
	}
	if strings.TrimSpace(tokens.IDToken) == "" || strings.TrimSpace(tokens.AccessToken) == "" || strings.TrimSpace(tokens.RefreshToken) == "" {
		return authPayload{}, errors.New("token exchange response is missing required tokens")
	}

	apiKey, _ := exchangeIDTokenForAPIKey(client, cfg, tokenEndpoint, tokens.IDToken)
	accountID := extractAccountIDFromIDToken(tokens.IDToken)
	mode := "chatgpt"
	now := nowFunc().UTC()
	return authPayload{
		AuthMode:     &mode,
		OpenAIAPIKey: apiKey,
		Tokens: &tokenData{
			IDToken:      tokens.IDToken,
			AccessToken:  tokens.AccessToken,
			RefreshToken: tokens.RefreshToken,
			AccountID:    accountID,
		},
		LastRefresh: &now,
	}, nil
}

func exchangeIDTokenForAPIKey(client *http.Client, cfg refreshConfig, tokenEndpoint, idToken string) (*string, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("client_id", clientID)
	form.Set("requested_token", "openai-api-key")
	form.Set("subject_token", idToken)
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:id_token")

	req, err := http.NewRequest(http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("originator", resolveOriginator(cfg.Originator))
	req.Header.Set("User-Agent", resolveUserAgent(cfg))

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("api key exchange failed: %s", resp.Status)
	}

	var parsed apiKeyExchangeResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return nil, errors.New("api key exchange response is missing access_token")
	}
	return &parsed.AccessToken, nil
}

func buildLoginAuthorizeURL(issuer, redirectURI, codeChallenge, state string) string {
	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", clientID)
	values.Set("redirect_uri", redirectURI)
	values.Set("scope", "openid profile email offline_access api.connectors.read api.connectors.invoke")
	values.Set("code_challenge", codeChallenge)
	values.Set("code_challenge_method", "S256")
	values.Set("id_token_add_organizations", "true")
	values.Set("codex_cli_simplified_flow", "true")
	values.Set("state", state)
	values.Set("originator", resolveOriginator(""))
	return strings.TrimRight(issuer, "/") + "/oauth/authorize?" + values.Encode()
}

func generatePKCE() (pkceCodes, error) {
	verifier, err := randomBase64URL(64)
	if err != nil {
		return pkceCodes{}, err
	}
	digest := sha256.Sum256([]byte(verifier))
	return pkceCodes{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(digest[:]),
	}, nil
}

func randomBase64URL(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func writeTemporaryLoginAuth(codexDir string, auth authPayload) (string, error) {
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		return "", err
	}

	raw, err := json.Marshal(auth)
	if err != nil {
		return "", err
	}

	var doc authDocument
	if err := json.Unmarshal(raw, &doc.raw); err != nil {
		return "", err
	}

	tempFile, err := os.CreateTemp(codexDir, "auth.json.login.*")
	if err != nil {
		return "", fmt.Errorf("create temp login auth file: %w", err)
	}
	tempPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}

	if err := saveAuthDocument(tempPath, doc); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	return tempPath, nil
}

func extractAccountIDFromIDToken(jwt string) *string {
	claims, err := jwtAuthClaims(jwt)
	if err != nil {
		return nil
	}
	accountID, _ := claims["chatgpt_account_id"].(string)
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	return &accountID
}

func jwtAuthClaims(jwt string) (map[string]any, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, err
	}
	claims, _ := root["https://api.openai.com/auth"].(map[string]any)
	return claims, nil
}

func openBrowser(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		if text := strings.TrimSpace(stderr.String()); text != "" {
			return fmt.Errorf("%w: %s", err, text)
		}
		return err
	}
	return nil
}
