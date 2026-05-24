# telegram-to-git

Telegram bot that saves messages from chats and channels into a git repository. Text goes into daily `.txt` files; photos, videos, and documents are downloaded and committed alongside them.

## File structure

```
telegram/
  {channel-name}/
    {year}/
      {year}-{month}-{day}.txt
      media/
        {date}_{id}.jpg
        {date}_{id}.mp4
        ...
```

Example log line:
```
[2025-05-24 14:30:00] @username: Hello world
[2025-05-24 14:31:00] @username: [edited] Hello world (fixed)
[2025-05-24 14:32:00] @username: [Photo: telegram/journal/2025/media/2025-05-24_abc123.jpg (1.2 MB)] look at this
[2025-05-24 14:33:00] @username: [Document: 25.3 MB > 20 MB limit, skipped]
```

Edited messages are appended as new lines with an `[edited]` prefix. Deleted messages cannot be tracked — the Telegram Bot API does not send deletion events.

## Setup

### 1. Create a bot

Open [@BotFather](https://t.me/BotFather), run `/newbot`, copy the token.

For **groups**: add the bot as an admin with "Read messages" permission.  
For **channels**: add the bot as an admin.  
For **private chats**: just start a conversation with the bot.

### 2. Configure

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml`:

```yaml
telegram_token: "1234567890:ABCdef..."

channels:
  - id: -1001234567890   # numeric ID (use @userinfobot to find it)
    name: "my-journal"   # becomes the folder name
  - username: "@mychannel"
    name: "my-channel"

repo_path: "/path/to/your/git/repo"

# Optional — defaults shown
git_branch: "main"          # auto-detected from repo if omitted
git_remote: "origin"
git_author_name: "Telegram Bot"
git_author_email: "bot@telegram"
max_file_size_mb: 20        # Telegram bots can't access files > 20 MB anyway
commit_delay_min: 2         # wait N min after last message before committing
commit_max_delay_min: 10    # commit regardless after this many minutes
push: true
```

**How the commit timer works:** after a message arrives, the bot waits `commit_delay_min` minutes. Each new message resets the timer. If messages keep arriving, the bot commits after `commit_max_delay_min` minutes from the first pending change, then the cycle restarts.

### 3. Build and run

```bash
go build -o telegram-to-git .
./telegram-to-git --config config.yaml
```

Or with a custom config path to run multiple instances simultaneously:

```bash
./telegram-to-git --config config-work.yaml &
./telegram-to-git --config config-personal.yaml &
```

### 4. Target repository

The repository at `repo_path` must already be initialised (`git init`) and, if `push: true`, have a remote configured. The bot handles conflicts automatically: it tries `pull --rebase` first, then falls back to `--force` push if the rebase fails.

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

## Notes

- Media size limit defaults to 20 MB — the hard ceiling imposed by the Telegram Bot API regardless of your setting.
- If `channels` is left empty, the bot accepts messages from every chat it receives updates from (handy for initial testing).
- The binary and `config.yaml` are excluded from git via `.gitignore` to keep credentials out of the repo.
