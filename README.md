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
```

The list shows how many `auth.json.*` backups are available, along with relative ages
such as `3h ago` or `3d ago` for each file's modified time, access time (when supported
by the filesystem), and `last_refresh` timestamp from the auth payload.

Open the interactive selector:

```bash
switch-codex-auth
```

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
```

`refresh` only updates alias files, never the active `auth.json`. It skips aliases that
are not refreshable and prints a summary of refreshed, skipped, and failed files.

## Auth Directory

By default the tool reads auth files from:

- macOS / Linux: `$HOME/.codex`
- Windows: `%USERPROFILE%\.codex`

You can override the directory with `CODEX_HOME`.

## Behavior

- Scans `auth.json.*` files and lists them by suffix.
- Shows the number of available auth files plus relative ages for each backup's modified time, access time, and `last_refresh` when present.
- Detects which backup currently matches `auth.json`.
- Replaces `auth.json` through a temp file in the same directory before renaming it into place.
- Supports `list`, `use`, `save`, and `refresh` as explicit subcommands.
- Saves a new alias with `save <suffix>`, prompting before overwriting an existing `auth.json.<suffix>` in interactive terminals.
- Supports `-f` / `--force` with `save` to overwrite an existing alias without prompting.
- Refreshes all refreshable `auth.json.*` aliases with `refresh`, grouping identical `refresh_token` values so the same token is refreshed only once.
- Supports number selection or suffix selection in interactive mode.
