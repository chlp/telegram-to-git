# telegram-to-git

Telegram bot that saves messages from chats and channels into a git repository. Each message is one line in a daily `.txt` file; photos, videos, and documents are downloaded alongside. Edits update the line in-place — the git history shows exactly what changed and when.

## File structure

```
telegram/
  {channel-name}/
    .msgindex                      ← internal: maps message ID to date
    {year}/
      {year}-{month}-{day}.txt
      media/
        {date}_{id}.jpg
        {date}_{id}.mp4
        ...
```

## Message format

Each message is a single line:

```
[2025-05-24 14:30:00 #12345] @username: Hello world
```

**Edited message** — the line is updated in-place, an `edit:` tag appears in the header:

```
[2025-05-24 14:30:00 #12345 edit:14:41:00] @username: Hello world, fixed
```

Multiple edits update the same tag — only the latest edit time is shown in the file. The full edit history (every intermediate version) is visible via `git log -p`.

**Media messages:**

```
[2025-05-24 14:32:00 #12346] @username: [Photo: telegram/chan/2025/media/2025-05-24_abc123.jpg (1.2 MB)] optional caption
[2025-05-24 14:33:00 #12347] @username: [Video: telegram/chan/2025/media/2025-05-24_def456.mp4 (8.4 MB)]
[2025-05-24 14:34:00 #12348] @username: [Document: 25.3 MB > 20 MB limit, skipped]
```

Newlines inside message text are stored as `\n` so the one-line-per-message format is preserved.

**Deleted messages** — the Telegram Bot API does not send deletion events to bots. Deletions cannot be tracked without a userbot (MTProto).

## Reviewing history with Claude

Because every edit is a line replacement committed to git, you can ask Claude to analyse changes using standard git tools:

```bash
# What changed in a file over time
git log -p telegram/journal/2025/2025-05-24.txt

# All edits committed in the last week
git log --since="1 week ago" -p --grep="telegram:"

# What a specific message originally said
git log -p --all -S "#12345"
```

## Setup

### 1. Create a bot

Open [@BotFather](https://t.me/BotFather), run `/newbot`, copy the token.

For **groups**: add the bot as admin with "Read messages" permission.  
For **channels**: add the bot as admin.  
For **private chats**: start a conversation with the bot.

To find a chat's numeric ID, forward a message from it to [@userinfobot](https://t.me/userinfobot).

### 2. Configure

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml`:

```yaml
telegram_token: "1234567890:ABCdef..."

channels:
  - id: -1001234567890   # numeric ID (groups and channels have negative IDs)
    name: "my-journal"   # becomes the folder name under telegram/
  - username: "@mychannel"
    name: "my-channel"
  - id: 987654321        # private chat with a user
    name: "notes"

repo_path: "/path/to/your/git/repo"

# Optional — defaults shown below
git_branch: "main"            # auto-detected from repo if omitted
git_remote: "origin"
git_author_name: "Telegram Bot"
git_author_email: "bot@telegram"
max_file_size_mb: 20          # Telegram bots cannot access files > 20 MB regardless
commit_delay_min: 2           # reset wait timer after each message or edit
commit_max_delay_min: 10      # commit regardless after this many minutes
push: true
```

**Commit timer:** after any activity the bot waits `commit_delay_min` minutes. Each new message or edit resets the timer. If activity keeps resetting the timer, the bot commits after `commit_max_delay_min` minutes from the first pending change and the cycle restarts. This batches rapid message bursts into a single commit.

### 3. Target repository

The repository at `repo_path` must be initialised (`git init`) and, if `push: true`, have a remote configured. The bot resolves push conflicts automatically: `pull --rebase` first, then `--force` push if the rebase fails.

### 4. Build and run

```bash
go build -o telegram-to-git .
./telegram-to-git --config config.yaml
```

### Multiple instances

Run with different config files to archive several channels into different repositories simultaneously:

```bash
./telegram-to-git --config config-work.yaml &
./telegram-to-git --config config-personal.yaml &
```

If `channels` is left empty the bot accepts messages from every chat it receives updates from — useful for initial testing.

## Running as a service (macOS launchd)

Create `~/Library/LaunchAgents/com.telegram-to-git.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>             <string>com.telegram-to-git</string>
  <key>ProgramArguments</key>
  <array>
    <string>/path/to/telegram-to-git</string>
    <string>--config</string>
    <string>/path/to/config.yaml</string>
  </array>
  <key>RunAtLoad</key>         <true/>
  <key>KeepAlive</key>         <true/>
  <key>StandardOutPath</key>   <string>/tmp/telegram-to-git.log</string>
  <key>StandardErrorPath</key> <string>/tmp/telegram-to-git.log</string>
</dict>
</plist>
```

```bash
launchctl load ~/Library/LaunchAgents/com.telegram-to-git.plist
```
