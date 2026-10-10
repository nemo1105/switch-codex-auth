# switch-codex-auth

A small cross-platform CLI for switching Codex auth profiles.

## Install

Install from source in the repository:

```bash
go install .
```

Install from GitHub:

```bash
go install github.com/nemo1105/switch-codex-auth@latest
```

## Usage

Show the current auth profile and available backups:

```bash
switch-codex-auth list
switch-codex-auth list --usage chat
switch-codex-auth list --usage chat --model gpt-5.6-luna
switch-codex-auth list --usage api
```

The list shows how many `auth.json.*` backups are available, along with relative ages
such as `3h ago` or `3d ago` for each file's `last_refresh` timestamp from the auth
payload. Usage is not fetched by default and displays as `-`. Use `--usage chat` to
fetch usage by sending a minimal Codex request and reading quota headers, or `--usage api`
to use the direct usage endpoint. Chat probes use `gpt-5.6-luna` by default; pass
`--model <name>` to use a different model for the probe.

Open the interactive selector:

```bash
switch-codex-auth
switch-codex-auth --usage chat
switch-codex-auth --usage chat --model gpt-5.6-luna
```

The interactive view shows the same table before prompting for a selection. When usage is
requested and a profile has remaining 5-hour usage, pressing `Enter` selects the default
profile with the most 5-hour remaining usage, using 7-day remaining usage as the tie
breaker. If usage is not requested, every profile has 0% 5-hour remaining usage, or usage
is unavailable, no default is shown and an empty selection prompts again.
After the list is shown, interactive mode refreshes stale aliases in the background.
Selection remains available while refresh runs; after selection, the tool waits for any
in-progress refresh, prints the refresh results, and syncs the active `auth.json` if the
selected alias was refreshed.

Switch directly to a suffix:

```bash
switch-codex-auth use tech
switch-codex-auth use 11
switch-codex-auth use auth.json.wcl
```

### Switching with the Codex background server

Recent Codex CLI versions can reuse a shared local background server. Replacing
`auth.json` does not necessarily reload that server's in-memory credentials:
`switch-codex-auth` may select account B while a new terminal running `codex`
still shows account A in `/status`.

After a successful selection, this tool checks `codex app-server daemon version`
using the same `CODEX_HOME`. If a server is running, the default behavior prints
recovery instructions without interrupting other tasks. To apply the switch to
the shared server too, explicitly opt into a restart:

```bash
switch-codex-auth use tech --restart-daemon
switch-codex-auth --restart-daemon
switch-codex-auth --usage chat --restart-daemon
```

**Restarting the shared server interrupts its running tasks and disconnects its
clients.** Wait for other tasks to finish before using this option. Interactive
mode restarts only after background alias refresh has finished and the selected
credentials have been synced to `auth.json`. The option also works when the
selected alias already matches `auth.json`, so a previously stale server can be
restarted without selecting another alias first.

Alternatively, use `codex --no-daemon` after switching to start an independent
process with the selected file credentials, or manually run
`codex app-server daemon restart` when ready. Open Codex and run `/status` to
verify the account afterward.

The check does not start a daemon. Default switching still works when Codex is
not installed or the status command is unavailable. With `--restart-daemon`, a
failed status check, restart, or verification returns an error that explicitly
states that `auth.json` remains selected; the file is not rolled back. Only a
server reported as running is restarted. These commands were verified with
Codex CLI 0.162.1. The tool manages file credentials; accounts held by the desktop
app or an OS credential store may need separate handling.

Save the current `auth.json` as a new alias:

```bash
switch-codex-auth save demo
switch-codex-auth save auth.json.backup-20260319
switch-codex-auth save demo --force
```

If the alias already exists, interactive terminals prompt you to press `Enter` to overwrite
or type a different alias to save under. In non-interactive environments, use `--force`
with `save` to overwrite an existing alias.

Sign in with Codex OAuth and save the new credentials as an alias:

```bash
switch-codex-auth login
switch-codex-auth login demo
switch-codex-auth login demo --force
switch-codex-auth login demo -p
switch-codex-auth login demo --print-url-only
```

`login` prints and opens the Codex OAuth URL, waits for browser sign-in, then saves the
new credentials as `auth.json.<suffix>`. Use `-p` or `--print-url-only` to print the URL
without opening a browser automatically. It does not revoke, overwrite, or otherwise
modify the active `auth.json`.

Refresh every refreshable `auth.json.*` alias:

```bash
switch-codex-auth refresh
switch-codex-auth refresh -f
switch-codex-auth refresh --days 30
```

`refresh` only updates alias files, never the active `auth.json`. It skips aliases that
are not refreshable, and by default only refreshes aliases whose `last_refresh` is at
least 7 days old or missing. Use `-f` / `--force` to force-refresh every refreshable
alias without checking `last_refresh`, or use `--days N` to override the threshold.

## Auth Directory

By default the tool reads auth files from:

- macOS / Linux: `$HOME/.codex`
- Windows: `%USERPROFILE%\.codex`

You can override the directory with `CODEX_HOME`.

## Behavior

- Scans `auth.json.*` files and lists them by suffix.
- Shows the number of available auth files, marks the current alias with `*` in the index column, and displays relative `last_refresh` time when present.
- Leaves usage blank by default. `--usage chat` fetches usage for ChatGPT-backed aliases via a minimal Codex request using `gpt-5.6-luna` by default; `--model <name>` overrides that probe model, while `--usage api` uses the direct usage endpoint. Rows show a compact remaining-quota summary, `n/a`, or a concise status/message error when usage is unavailable.
- Detects which backup currently matches `auth.json`.
- Replaces `auth.json` through a temp file in the same directory before renaming it into place.
- Checks for a shared Codex background server after selection and prints guidance when it may retain the previous account. `--restart-daemon` explicitly restarts a running server after the final auth-file sync.
- Supports `list`, `use`, `save`, `login`, and `refresh` as explicit subcommands.
- Saves a new alias with `save <suffix>`, prompting before overwriting an existing `auth.json.<suffix>` in interactive terminals.
- Saves a new OAuth login with `login [suffix]`, prompting for the suffix after sign-in when omitted.
- Supports `-f` / `--force` with `save` to overwrite an existing alias without prompting, and with `refresh` to skip `last_refresh` checks.
- Refreshes `auth.json.*` aliases with `refresh`, defaulting to entries whose `last_refresh` is at least 7 days old or missing, and grouping identical `refresh_token` values so the same token is refreshed only once.
- Supports number selection, suffix selection, usage-based default selection, and background stale-alias refresh in interactive mode.
