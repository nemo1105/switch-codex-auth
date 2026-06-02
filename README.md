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
switch-codex-auth list --usage api
```

The list shows how many `auth.json.*` backups are available, along with relative ages
such as `3h ago` or `3d ago` for each file's `last_refresh` timestamp from the auth
payload. Usage is not fetched by default and displays as `-`. Use `--usage chat` to
fetch usage by sending a minimal Codex request and reading quota headers, or `--usage api`
to use the direct usage endpoint.

Open the interactive selector:

```bash
switch-codex-auth
switch-codex-auth --usage chat
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

Save the current `auth.json` as a new alias:

```bash
switch-codex-auth save demo
switch-codex-auth save auth.json.backup-20260319
switch-codex-auth save demo --force
```

If the alias already exists, interactive terminals prompt you to press `Enter` to overwrite
or type a different alias to save under. In non-interactive environments, use `--force`
with `save` to overwrite an existing alias.

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
- Leaves usage blank by default. `--usage chat` fetches usage for ChatGPT-backed aliases via a minimal Codex request, and `--usage api` uses the direct usage endpoint; rows show a compact remaining-quota summary, `n/a`, or a concise status/message error when usage is unavailable.
- Detects which backup currently matches `auth.json`.
- Replaces `auth.json` through a temp file in the same directory before renaming it into place.
- Supports `list`, `use`, `save`, and `refresh` as explicit subcommands.
- Saves a new alias with `save <suffix>`, prompting before overwriting an existing `auth.json.<suffix>` in interactive terminals.
- Supports `-f` / `--force` with `save` to overwrite an existing alias without prompting, and with `refresh` to skip `last_refresh` checks.
- Refreshes `auth.json.*` aliases with `refresh`, defaulting to entries whose `last_refresh` is at least 7 days old or missing, and grouping identical `refresh_token` values so the same token is refreshed only once.
- Supports number selection, suffix selection, usage-based default selection, and background stale-alias refresh in interactive mode.
