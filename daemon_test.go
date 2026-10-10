package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func stubDaemonCommand(t *testing.T, fn func(string, time.Duration, string) ([]byte, error)) {
	t.Helper()
	old := runCodexDaemonCommandFunc
	runCodexDaemonCommandFunc = fn
	t.Cleanup(func() { runCodexDaemonCommandFunc = old })
}

func TestDaemonSwitchPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		restart    bool
		status     string
		checkErr   error
		restartErr error
		wantErr    bool
		wantCalls  string
		wantOutput string
	}{
		{"running defaults to guidance", false, `{"status":"running"}`, nil, nil, false, "version", "even in a new terminal"},
		{"running explicitly restarted", true, `{"status":"running"}`, nil, nil, false, "version,restart,version", "server restarted"},
		{"stopped never started", true, `{"status":"stopped"}`, nil, nil, false, "version", "no restart needed"},
		{"missing Codex remains optional", false, "", exec.ErrNotFound, nil, false, "version", ""},
		{"explicit missing Codex reports partial success", true, "", exec.ErrNotFound, nil, true, "version", "auth.json is selected"},
		{"unsupported status preserves default switch", false, "", errors.New("unsupported"), nil, false, "version", "--no-daemon"},
		{"explicit unsupported status does not restart", true, "", errors.New("unsupported"), nil, true, "version", "could not check"},
		{"malformed status never restarts", true, `not-json`, nil, nil, true, "version", "invalid daemon status"},
		{"unknown status never restarts", true, `{"status":"unexpected"}`, nil, nil, true, "version", "unknown daemon status"},
		{"restart failure reports partial success", true, `{"status":"running"}`, nil, errors.New("failed"), true, "version,restart", "restart failed"},
		{"restart timeout reports partial success", true, `{"status":"running"}`, nil, context.DeadlineExceeded, true, "version,restart", "deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			stubDaemonCommand(t, func(dir string, timeout time.Duration, action string) ([]byte, error) {
				if dir != "selected-home" {
					t.Fatalf("wrong CODEX_HOME: %q", dir)
				}
				calls = append(calls, action)
				if action == "restart" {
					return nil, tc.restartErr
				}
				return []byte(tc.status), tc.checkErr
			})
			var out bytes.Buffer
			err := handleCodexDaemon("selected-home", tc.restart, &out)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if got := strings.Join(calls, ","); got != tc.wantCalls {
				t.Fatalf("commands = %s, want %s", got, tc.wantCalls)
			}
			message := out.String()
			if err != nil {
				message += err.Error()
			}
			if !strings.Contains(message, tc.wantOutput) {
				t.Fatalf("missing %q in %q", tc.wantOutput, message)
			}
		})
	}
}

func TestDaemonRestartMustBeVerified(t *testing.T) {
	for _, verification := range []struct {
		name, status string
		err          error
	}{
		{"stopped", `{"status":"stopped"}`, nil},
		{"status failure", "", errors.New("unavailable")},
	} {
		t.Run(verification.name, func(t *testing.T) {
			checks := 0
			stubDaemonCommand(t, func(_ string, _ time.Duration, action string) ([]byte, error) {
				if action == "restart" {
					return nil, nil
				}
				checks++
				if checks == 1 {
					return []byte(`{"status":"running"}`), nil
				}
				return []byte(verification.status), verification.err
			})
			var out bytes.Buffer
			err := handleCodexDaemon("home", true, &out)
			if err == nil || !strings.Contains(err.Error(), "could not be verified") {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(out.String(), "server restarted.") {
				t.Fatal("reported unverified success")
			}
		})
	}
}

func TestUseRestartDaemonCLI(t *testing.T) {
	for _, args := range [][]string{
		{"use", "demo", "--restart-daemon"},
		{"use", "--restart-daemon", "demo"},
		{"use", "demo", "--restart-daemon=true"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			writeAuthFile(t, filepath.Join(dir, "auth.json"), "old-auth")
			writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "selected-auth")
			var calls []string
			stubDaemonCommand(t, func(home string, _ time.Duration, action string) ([]byte, error) {
				if home != dir {
					t.Fatal("wrong home")
				}
				if got := readAuthFile(t, filepath.Join(home, "auth.json")); got != "selected-auth" {
					t.Fatal("daemon called before switch")
				}
				calls = append(calls, action)
				return []byte(`{"status":"running"}`), nil
			})
			var out bytes.Buffer
			if err := runCLI(args, strings.NewReader(""), &out); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(calls, ","); got != "version,restart,version" {
				t.Fatalf("commands = %s", got)
			}
		})
	}
}

func TestUseRestartDaemonFlagRespectsFalseAndEndOfOptions(t *testing.T) {
	for _, args := range [][]string{
		{"demo", "--restart-daemon=false"},
		{"--", "--restart-daemon"},
	} {
		var out bytes.Buffer
		selection, restart, handled, err := parseUseSubcommandArgs(args, &out, "switch-codex-auth")
		if err != nil || handled || restart {
			t.Fatalf("args %v: restart=%v, handled=%v, error=%v", args, restart, handled, err)
		}
		if selection != args[0] && selection != "--restart-daemon" {
			t.Fatalf("selection = %q", selection)
		}
	}
}

func TestAlreadySelectedProfileCanRestartStaleDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	auth := map[string]any{"tokens": map[string]any{"id_token": testJWT(t, map[string]any{"email": "selected@example.com"})}}
	writeJSONFile(t, filepath.Join(dir, "auth.json"), auth)
	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), auth)
	restarted := false
	stubDaemonCommand(t, func(_ string, _ time.Duration, action string) ([]byte, error) {
		if action == "restart" {
			restarted = true
		}
		return []byte(`{"status":"running"}`), nil
	})
	var out bytes.Buffer
	if err := runCLI([]string{"use", "demo", "--restart-daemon"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !restarted || !strings.Contains(out.String(), "Already using: demo") {
		t.Fatal(out.String())
	}
}

func TestFailedRestartPreservesSelectedAuth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeAuthFile(t, filepath.Join(dir, "auth.json"), "old-auth")
	writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "selected-auth")
	stubDaemonCommand(t, func(_ string, _ time.Duration, action string) ([]byte, error) {
		if action == "restart" {
			return nil, errors.New("failed")
		}
		return []byte(`{"status":"running"}`), nil
	})
	var out bytes.Buffer
	err := runCLI([]string{"use", "demo", "--restart-daemon"}, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "auth.json is selected") {
		t.Fatalf("error = %v", err)
	}
	if got := readAuthFile(t, filepath.Join(dir, "auth.json")); got != "selected-auth" {
		t.Fatalf("auth rolled back: %s", got)
	}
}

func TestDaemonIsNotTouchedForInvalidSelectionOrOtherCommands(t *testing.T) {
	for _, args := range [][]string{
		{"use", "missing", "--restart-daemon"},
		{"use", "--restart-daemon"},
		{"use", "demo", "--restart-daemon=invalid"},
		{"list"}, {"save", "saved"}, {"help"}, {"use", "--help"},
		{"list", "--restart-daemon"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			writeAuthFile(t, filepath.Join(dir, "auth.json"), "old-auth")
			writeAuthFile(t, filepath.Join(dir, "auth.json.demo"), "selected-auth")
			stubDaemonCommand(t, func(_ string, _ time.Duration, _ string) ([]byte, error) {
				t.Fatal("unexpected daemon command")
				return nil, nil
			})
			var out bytes.Buffer
			_ = runCLI(args, strings.NewReader(""), &out)
			if got := readAuthFile(t, filepath.Join(dir, "auth.json")); got != "old-auth" {
				t.Fatal("unexpected auth switch")
			}
		})
	}
}

func TestInteractiveDaemonRestartWaitsForRefreshedAuth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeAuthFile(t, filepath.Join(dir, "auth.json"), "old-auth")
	writeJSONFile(t, filepath.Join(dir, "auth.json.demo"), map[string]any{
		"tokens": map[string]any{"refresh_token": "test-refresh-token"},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(t, w, map[string]any{"access_token": "refreshed-access", "refresh_token": "refreshed-refresh", "id_token": "refreshed-id"})
	}))
	t.Cleanup(server.Close)
	t.Setenv(refreshTokenURLOverrideEnv, server.URL)
	var calls []string
	stubDaemonCommand(t, func(home string, _ time.Duration, action string) ([]byte, error) {
		active := readJSONFile(t, filepath.Join(home, "auth.json"))
		if nestedMap(t, active, "tokens")["access_token"] != "refreshed-access" {
			t.Fatal("daemon called before refreshed auth was synced")
		}
		calls = append(calls, action)
		return []byte(`{"status":"running"}`), nil
	})
	var out bytes.Buffer
	if err := runCLI([]string{"--restart-daemon"}, strings.NewReader("demo\n"), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "version,restart,version" {
		t.Fatal(calls)
	}
}

func TestDaemonCommandEnvironmentAndTimeout(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "helper.go")
	program := `package main
import ("encoding/json"; "os"; "time")
func main() {
 if os.Getenv("SWITCH_CODEX_TEST_SLEEP") == "1" { time.Sleep(time.Minute) }
 json.NewEncoder(os.Stdout).Encode(map[string]any{"args":os.Args[1:], "home":os.Getenv("CODEX_HOME")})
}`
	if err := os.WriteFile(source, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	build := exec.Command("go", "build", "-o", filepath.Join(dir, name), source)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v: %s", err, output)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", "inherited-home")
	data, err := runCodexDaemonCommand("selected-home", 5*time.Second, "version")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Args []string `json:"args"`
		Home string   `json:"home"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Home != "selected-home" || fmt.Sprint(result.Args) != "[app-server daemon version]" {
		t.Fatalf("unexpected child: %+v", result)
	}
	t.Setenv("SWITCH_CODEX_TEST_SLEEP", "1")
	_, err = runCodexDaemonCommand("selected-home", 100*time.Millisecond, "version")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
}
