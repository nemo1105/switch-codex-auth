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
switch-codex-auth --list
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
switch-codex-auth --use tech
switch-codex-auth --use 11
switch-codex-auth --use auth.json.wcl
```

Save the current `auth.json` as a new alias:

```bash
switch-codex-auth --save demo
switch-codex-auth --save auth.json.backup-20260319
```

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
- Saves a new alias with `--save <suffix>` without overwriting an existing `auth.json.<suffix>`.
- Supports number selection or suffix selection in interactive mode.
