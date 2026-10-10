package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const daemonRecoveryHint = "Start Codex with `codex --no-daemon`, or run `codex app-server daemon restart` after other tasks finish."

var runCodexDaemonCommandFunc = runCodexDaemonCommand

// Only the documented status command is used to detect the daemon. It does not
// start a server, and a restart is never attempted without explicit opt-in.
func handleCodexDaemon(codexDir string, restart bool, out io.Writer) error {
	running, err := codexDaemonRunning(codexDir)
	if err != nil {
		if restart {
			return fmt.Errorf("auth.json is selected, but could not check the Codex daemon: %w. %s", err, daemonRecoveryHint)
		}
		if !errors.Is(err, exec.ErrNotFound) {
			fmt.Fprintln(out, "Note: could not check the Codex background server; it may still use the previous account.")
			fmt.Fprintln(out, daemonRecoveryHint)
		}
		return nil
	}
	if !running {
		if restart {
			fmt.Fprintln(out, "No running Codex background server; no restart needed.")
		}
		return nil
	}
	if !restart {
		fmt.Fprintln(out, "Note: a Codex background server is running and may still use the previous account, even in a new terminal.")
		fmt.Fprintln(out, daemonRecoveryHint)
		fmt.Fprintln(out, "To restart it as part of a switch, use --restart-daemon (interrupts running tasks).")
		return nil
	}

	fmt.Fprintln(out, "Restarting the Codex background server to reload auth.json (interrupts running tasks)...")
	if _, err := runCodexDaemonCommandFunc(codexDir, 30*time.Second, "restart"); err != nil {
		return fmt.Errorf("auth.json is selected, but the Codex daemon restart failed: %w. %s", err, daemonRecoveryHint)
	}
	running, err = codexDaemonRunning(codexDir)
	if err != nil || !running {
		if err == nil {
			err = errors.New("daemon is not running")
		}
		return fmt.Errorf("auth.json is selected, but the restarted Codex daemon could not be verified: %w. %s", err, daemonRecoveryHint)
	}
	fmt.Fprintln(out, "Codex background server restarted. Open Codex and run /status to verify the selected account.")
	return nil
}

func codexDaemonRunning(codexDir string) (bool, error) {
	data, err := runCodexDaemonCommandFunc(codexDir, 5*time.Second, "version")
	if err != nil {
		return false, err
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return false, fmt.Errorf("invalid daemon status: %w", err)
	}
	switch status.Status {
	case "running":
		return true, nil
	case "stopped", "not_running":
		return false, nil
	default:
		return false, errors.New("unknown daemon status")
	}
}

func runCodexDaemonCommand(codexDir string, timeout time.Duration, action string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "app-server", "daemon", action)
	// Match the directory whose auth.json was replaced, including CODEX_HOME
	// overrides. Never reuse an inherited CODEX_HOME for a different directory.
	for _, entry := range os.Environ() {
		if key, _, ok := strings.Cut(entry, "="); ok && strings.EqualFold(key, "CODEX_HOME") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+codexDir)
	// Child diagnostics can contain account details. Do not forward them.
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	data, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return data, err
}
