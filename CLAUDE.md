# CLAUDE.md

## What this is

`telegram-to-git` is a single-binary Go service that listens to Telegram chats/channels via the Bot API and appends every message to daily `.txt` files in a git repository, committing and pushing on a configurable debounce schedule.

## Architecture

Everything lives in `main.go` — no packages, no layers. Keep it that way unless the file grows past ~600 lines.

Key types:
- `Config` — parsed from YAML config file, one per running instance
- `Channel` — a chat to monitor (by numeric ID or @username) with a folder `Name`
- `Bot` — holds the Telegram API client, commit timer state, and a map of known chat IDs

### Commit scheduling (`scheduleCommit`)

Two-tier debounce to avoid excessive git operations:
- `commit_delay_min` — timer resets on each new event (default 2 min)
- `commit_max_delay_min` — hard deadline from first pending change (default 10 min)

`syncMu` (a second mutex) serialises concurrent `gitSync` calls that can arise when the max-delay path fires a goroutine while the timer path is also active.

### Git conflict strategy (`gitSync`)

1. `git add -A` + `git commit`
2. `git push` → if fails: `git pull --rebase` → `git push`
3. If rebase fails: `git rebase --abort` + `git push --force`

The repo this writes to is the **user's notes repo**, not this source repo.

## Config file

`config.yaml` is gitignored (contains the bot token). Always work from `config.example.yaml` as the canonical reference. All fields have safe defaults — only `telegram_token` and `repo_path` are required.

## Building

```bash
go build -o telegram-to-git .
```

No Makefile, no Docker, no build tags. `go mod tidy` if dependencies drift.

## Running multiple instances

Supported by design — pass `--config path/to/config.yaml`. Each instance is independent; they must point to different repos or at least different channel sets writing to non-overlapping paths.

## What the Bot API cannot do

- **Deleted messages**: the API sends no deletion events. Not implemented, not workaroundable without a userbot.
- **Files > 20 MB**: Telegram hard-limits bot file downloads to 20 MB. Files above the limit are noted in the log as skipped, never downloaded.

## Conventions

- No comments unless the WHY is non-obvious.
- No error wrapping with `fmt.Errorf("...: %w", ...)` — just log and return.
- Media filenames: `{date}_{last12charsOfFileID}.{ext}` — deterministic enough to deduplicate on re-edits.
