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

## Auth Directory

By default the tool reads auth files from:

- macOS / Linux: `$HOME/.codex`
- Windows: `%USERPROFILE%\.codex`

You can override the directory with `CODEX_HOME`.

## Behavior

- Scans `auth.json.*` files and lists them by suffix.
- Detects which backup currently matches `auth.json`.
- Replaces `auth.json` through a temp file in the same directory before renaming it into place.
- Supports number selection or suffix selection in interactive mode.
