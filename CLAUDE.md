# CLAUDE.md

## What this is

`telegram-to-git` is a single-binary Go service that listens to Telegram chats/channels via the Bot API and saves every message to daily `.txt` files in a git repository, committing and pushing on a configurable debounce schedule.

## File layout

```
telegram/
  {channel-name}/
    .msgindex            ← ID→date index for cross-day edit resolution
    {year}/
      {year}-{month}-{day}.txt
      media/
        {date}_{shortFileID}.{ext}
```

## Message format

Each message occupies **exactly one line**:

```
[YYYY-MM-DD HH:MM:SS #ID] @sender: content
```

An edited message has an `edit:HH:MM:SS` tag added to (or updated in) the header — the content is replaced in-place:

```
[2025-05-24 14:30:00 #12345 edit:14:41:00] @username: corrected text
```

Rules:
- Newlines inside message text are escaped as `\n` (literal backslash-n) to preserve the one-line invariant.
- Media is referenced as `[Photo: telegram/chan/2025/media/file.jpg (1.2 MB)]`.
- Files over the size limit are noted as `[Photo: 25.3 MB > 20 MB limit, skipped]`.
- The full edit history is in `git log -p` — each commit shows exactly which line changed and how.

## Index file (`.msgindex`)

Plain text, one entry per line: `{msgID} {date}`.

Used by `handleEdit` to find which day-file contains a given message ID (edits can arrive days after the original message). Appended on every `handleNew`; scanned linearly on `handleEdit`. For personal use (~50 msg/day) linear scan stays fast for years.

## Architecture

Everything lives in `main.go` — no packages, no layers. Keep it that way unless the file grows past ~700 lines.

Key types:
- `Config` — parsed from YAML; one per running process.
- `Channel` — a chat to monitor (numeric ID or @username) with a folder `Name`.
- `Bot` — Telegram API client, commit timer state, known chat ID map.

### Edit handling (`handleEdit`)

1. Look up original date in `.msgindex` (fallback: `msg.Date`).
2. Read the day-file, find the line containing ` #ID]`.
3. Strip any existing `edit:...` tag from the header, insert new one.
4. Replace content after `sender: ` with the new text/media reference.
5. Write the file back.

### Commit scheduling (`scheduleCommit`)

Two-tier debounce — avoids excessive git traffic from message bursts:
- `CommitDelayMin`: resets on each new event.
- `CommitMaxDelayMin`: hard deadline from first pending change.

`syncMu` serialises concurrent `gitSync` goroutines (the max-delay path can fire a goroutine while a timer-path goroutine is still running).

### Git conflict strategy (`gitSync`)

1. `git add -A` + `git commit`
2. `git push` → if fails: `git pull --rebase` → `git push`
3. If rebase fails: `git rebase --abort` + `git push --force`

## What Bot API cannot do

- **Deleted messages**: no deletion events are sent to bots. Would require a userbot (MTProto). Not implemented.
- **Files > 20 MB**: hard Telegram limit for bot downloads regardless of `max_file_size_mb`.

## Building

```bash
go build -o telegram-to-git .
```

No Makefile, no Docker. `go mod tidy` if dependencies drift.

## Running multiple instances

Pass `--config path/to/config.yaml`. Instances must write to different repos or non-overlapping channel paths to avoid git conflicts.

## Conventions

- No comments unless the WHY is non-obvious.
- `escNL()` is called on all user-supplied text to keep the one-line-per-message invariant.
- Media filenames are deterministic (`{date}_{last12ofFileID}.{ext}`) so `saveMedia` is idempotent and won't re-download on edit.
