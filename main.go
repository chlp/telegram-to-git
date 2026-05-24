package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gopkg.in/yaml.v3"
)

type Config struct {
	TelegramToken      string    `yaml:"telegram_token"`
	Channels           []Channel `yaml:"channels"`
	RepoPath           string    `yaml:"repo_path"`
	GitBranch          string    `yaml:"git_branch"`
	GitRemote          string    `yaml:"git_remote"`
	GitAuthorName      string    `yaml:"git_author_name"`
	GitAuthorEmail     string    `yaml:"git_author_email"`
	MaxFileSizeMB      float64   `yaml:"max_file_size_mb"`
	CommitDelayMin     float64   `yaml:"commit_delay_min"`
	CommitMaxDelayMin  float64   `yaml:"commit_max_delay_min"`
	Push               bool      `yaml:"push"`
}

type Channel struct {
	ID       int64  `yaml:"id"`
	Username string `yaml:"username"` // @username or username
	Name     string `yaml:"name"`     // folder name (latin, no spaces)
}

type Bot struct {
	cfg        *Config
	api        *tgbotapi.BotAPI
	mu         sync.Mutex // protects timer, hasDirty, firstDirty
	syncMu     sync.Mutex // serializes gitSync calls
	timer      *time.Timer
	hasDirty   bool
	firstDirty time.Time
	known      map[int64]string // chat ID -> folder name
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("read config %s: %v", *configPath, err)
	}

	cfg := Config{
		MaxFileSizeMB:     20,
		GitRemote:         "origin",
		CommitDelayMin:    2,
		CommitMaxDelayMin: 10,
		GitAuthorName:     "Telegram Bot",
		GitAuthorEmail:    "bot@telegram",
		Push:              true,
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if cfg.TelegramToken == "" {
		log.Fatal("telegram_token is required")
	}
	if cfg.RepoPath == "" {
		log.Fatal("repo_path is required")
	}
	if cfg.GitBranch == "" {
		cfg.GitBranch = detectBranch(cfg.RepoPath)
	}

	api, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		log.Fatalf("create bot: %v", err)
	}
	log.Printf("authorized as @%s", api.Self.UserName)
	log.Printf("commit delay: %.0f min, max: %.0f min", cfg.CommitDelayMin, cfg.CommitMaxDelayMin)

	b := &Bot{
		cfg:   &cfg,
		api:   api,
		known: make(map[int64]string),
	}
	for _, ch := range cfg.Channels {
		if ch.ID != 0 && ch.Name != "" {
			b.known[ch.ID] = ch.Name
		}
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	for update := range api.GetUpdatesChan(u) {
		switch {
		case update.Message != nil:
			b.handleMessage(update.Message, false)
		case update.ChannelPost != nil:
			b.handleMessage(update.ChannelPost, false)
		case update.EditedMessage != nil:
			b.handleMessage(update.EditedMessage, true)
		case update.EditedChannelPost != nil:
			b.handleMessage(update.EditedChannelPost, true)
		}
	}
}

func detectBranch(repoPath string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err == nil {
		if br := strings.TrimSpace(string(out)); br != "" && br != "HEAD" {
			return br
		}
	}
	return "main"
}

func (b *Bot) folderName(chat *tgbotapi.Chat) (string, bool) {
	if name, ok := b.known[chat.ID]; ok {
		return name, true
	}
	chatUser := strings.TrimPrefix(chat.UserName, "@")
	for _, ch := range b.cfg.Channels {
		u := strings.TrimPrefix(ch.Username, "@")
		if u != "" && u == chatUser {
			b.known[chat.ID] = ch.Name
			return ch.Name, true
		}
	}
	// No channels configured → accept all, derive name from chat metadata
	if len(b.cfg.Channels) == 0 {
		name := sanitize(chat.UserName)
		if name == "" {
			name = sanitize(chat.Title)
		}
		if name == "" {
			name = fmt.Sprintf("chat_%d", chat.ID)
		}
		b.known[chat.ID] = name
		return name, true
	}
	return "", false
}

func sanitize(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		case r == ' ':
			sb.WriteRune('-')
		}
	}
	return sb.String()
}

func (b *Bot) handleMessage(msg *tgbotapi.Message, edited bool) {
	folder, ok := b.folderName(msg.Chat)
	if !ok {
		return
	}

	ts := time.Unix(int64(msg.Date), 0).UTC()
	if edited && msg.EditDate != 0 {
		ts = time.Unix(int64(msg.EditDate), 0).UTC()
	}
	year := ts.Format("2006")
	date := ts.Format("2006-01-02")
	tsStr := ts.Format("2006-01-02 15:04:05")
	sender := senderStr(msg.From)

	dirPath := filepath.Join(b.cfg.RepoPath, "telegram", folder, year)
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		log.Printf("mkdir %s: %v", dirPath, err)
		return
	}

	prefix := ""
	if edited {
		prefix = "[edited] "
	}

	var entry string
	switch {
	case msg.Text != "":
		entry = fmt.Sprintf("[%s] %s: %s%s", tsStr, sender, prefix, msg.Text)

	case msg.Photo != nil:
		photo := msg.Photo[len(msg.Photo)-1]
		media := b.saveMedia(folder, year, date, "Photo", "jpg", photo.FileID, int64(photo.FileSize))
		entry = fmt.Sprintf("[%s] %s: %s%s%s", tsStr, sender, prefix, media, captionSuffix(msg.Caption))

	case msg.Video != nil:
		v := msg.Video
		media := b.saveMedia(folder, year, date, "Video", extFromMime(v.MimeType, "mp4"), v.FileID, int64(v.FileSize))
		entry = fmt.Sprintf("[%s] %s: %s%s%s", tsStr, sender, prefix, media, captionSuffix(msg.Caption))

	case msg.Document != nil:
		d := msg.Document
		ext := fileExt(d.FileName, extFromMime(d.MimeType, "bin"))
		media := b.saveMedia(folder, year, date, "Document", ext, d.FileID, int64(d.FileSize))
		entry = fmt.Sprintf("[%s] %s: %s%s%s", tsStr, sender, prefix, media, captionSuffix(msg.Caption))

	case msg.Audio != nil:
		a := msg.Audio
		media := b.saveMedia(folder, year, date, "Audio", extFromMime(a.MimeType, "mp3"), a.FileID, int64(a.FileSize))
		entry = fmt.Sprintf("[%s] %s: %s%s", tsStr, sender, prefix, media)

	case msg.Voice != nil:
		media := b.saveMedia(folder, year, date, "Voice", "ogg", msg.Voice.FileID, int64(msg.Voice.FileSize))
		entry = fmt.Sprintf("[%s] %s: %s%s", tsStr, sender, prefix, media)

	case msg.VideoNote != nil:
		media := b.saveMedia(folder, year, date, "VideoNote", "mp4", msg.VideoNote.FileID, int64(msg.VideoNote.FileSize))
		entry = fmt.Sprintf("[%s] %s: %s%s", tsStr, sender, prefix, media)

	case msg.Sticker != nil:
		s := msg.Sticker
		emoji := ""
		if s.Emoji != "" {
			emoji = s.Emoji + " "
		}
		media := b.saveMedia(folder, year, date, "Sticker", "webp", s.FileID, int64(s.FileSize))
		entry = fmt.Sprintf("[%s] %s: %s%s%s", tsStr, sender, prefix, emoji, media)

	default:
		return
	}

	txtFile := filepath.Join(dirPath, date+".txt")
	f, err := os.OpenFile(txtFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("open %s: %v", txtFile, err)
		return
	}
	_, werr := fmt.Fprintln(f, entry)
	f.Close()
	if werr != nil {
		log.Printf("write %s: %v", txtFile, werr)
		return
	}

	b.scheduleCommit()
}

// scheduleCommit arranges a git commit+push with two-tier delay:
//   - reset delay timer (CommitDelayMin) on each new event
//   - never wait more than CommitMaxDelayMin since first pending change
func (b *Bot) scheduleCommit() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	delay := time.Duration(float64(time.Minute) * b.cfg.CommitDelayMin)
	maxDelay := time.Duration(float64(time.Minute) * b.cfg.CommitMaxDelayMin)

	if !b.hasDirty {
		b.hasDirty = true
		b.firstDirty = now
	}

	elapsed := now.Sub(b.firstDirty)
	if elapsed >= maxDelay {
		// Max wait exceeded — push right away and start a new cycle
		if b.timer != nil {
			b.timer.Stop()
			b.timer = nil
		}
		b.hasDirty = false
		go func() {
			b.syncMu.Lock()
			defer b.syncMu.Unlock()
			b.gitSync()
		}()
		return
	}

	// Remaining time before max deadline
	remaining := maxDelay - elapsed
	d := delay
	if d > remaining {
		d = remaining
	}

	if b.timer != nil {
		b.timer.Reset(d)
	} else {
		b.timer = time.AfterFunc(d, func() {
			b.mu.Lock()
			b.timer = nil
			b.hasDirty = false
			b.mu.Unlock()
			b.syncMu.Lock()
			defer b.syncMu.Unlock()
			b.gitSync()
		})
	}
}

func (b *Bot) saveMedia(folder, year, date, kind, ext, fileID string, knownSize int64) string {
	fi, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		log.Printf("GetFile %s: %v", kind, err)
		return fmt.Sprintf("[%s: unavailable]", kind)
	}

	size := int64(fi.FileSize)
	if size == 0 {
		size = knownSize
	}
	limitBytes := int64(b.cfg.MaxFileSizeMB * 1024 * 1024)
	if size > 0 && size > limitBytes {
		return fmt.Sprintf("[%s: %.1f MB > %.0f MB limit, skipped]",
			kind, float64(size)/1048576, b.cfg.MaxFileSizeMB)
	}

	mediaDir := filepath.Join(b.cfg.RepoPath, "telegram", folder, year, "media")
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		log.Printf("mkdir media: %v", err)
		return fmt.Sprintf("[%s: storage error]", kind)
	}

	shortID := fileID
	if len(shortID) > 12 {
		shortID = shortID[len(shortID)-12:]
	}
	fileName := fmt.Sprintf("%s_%s.%s", date, shortID, ext)
	outPath := filepath.Join(mediaDir, fileName)
	relPath := filepath.Join("telegram", folder, year, "media", fileName)

	// Skip download if file already saved (e.g. edited message with same media)
	if _, err := os.Stat(outPath); err == nil {
		return fmt.Sprintf("[%s: %s]", kind, relPath)
	}

	resp, err := http.Get(fi.Link(b.api.Token)) //nolint:noctx
	if err != nil {
		log.Printf("download %s: %v", kind, err)
		return fmt.Sprintf("[%s: download failed]", kind)
	}
	defer resp.Body.Close()

	out, err := os.Create(outPath)
	if err != nil {
		log.Printf("create %s: %v", outPath, err)
		return fmt.Sprintf("[%s: write failed]", kind)
	}
	defer out.Close()

	n, err := io.Copy(out, resp.Body)
	if err != nil {
		log.Printf("write %s: %v", outPath, err)
		return fmt.Sprintf("[%s: write failed]", kind)
	}

	return fmt.Sprintf("[%s: %s (%.1f MB)]", kind, relPath, float64(n)/1048576)
}

func (b *Bot) gitSync() {
	dir := b.cfg.RepoPath
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME="+b.cfg.GitAuthorName,
		"GIT_AUTHOR_EMAIL="+b.cfg.GitAuthorEmail,
		"GIT_COMMITTER_NAME="+b.cfg.GitAuthorName,
		"GIT_COMMITTER_EMAIL="+b.cfg.GitAuthorEmail,
	)

	run := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return string(out), err
	}

	if _, err := run("add", "-A"); err != nil {
		return
	}

	status, _ := run("status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		return
	}

	msg := "telegram: " + time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	if _, err := run("commit", "-m", msg); err != nil {
		return
	}
	log.Printf("committed: %s", msg)

	if !b.cfg.Push {
		return
	}

	if _, err := run("push", b.cfg.GitRemote, b.cfg.GitBranch); err == nil {
		return
	}

	// Push failed — try pull --rebase, then push again
	if _, err := run("pull", "--rebase", b.cfg.GitRemote, b.cfg.GitBranch); err != nil {
		run("rebase", "--abort") //nolint:errcheck
		log.Println("rebase failed, force pushing")
		run("push", "--force", b.cfg.GitRemote, b.cfg.GitBranch) //nolint:errcheck
		return
	}

	if _, err := run("push", b.cfg.GitRemote, b.cfg.GitBranch); err != nil {
		log.Println("push failed after rebase, force pushing")
		run("push", "--force", b.cfg.GitRemote, b.cfg.GitBranch) //nolint:errcheck
	}
}

func senderStr(u *tgbotapi.User) string {
	if u == nil {
		return "anonymous"
	}
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if u.UserName != "" {
		return "@" + u.UserName
	}
	return name
}

func captionSuffix(c string) string {
	if c == "" {
		return ""
	}
	return " " + c
}

func extFromMime(mime, fallback string) string {
	switch mime {
	case "video/mp4":
		return "mp4"
	case "video/webm":
		return "webm"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/ogg":
		return "ogg"
	case "audio/wav":
		return "wav"
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "application/pdf":
		return "pdf"
	}
	if parts := strings.SplitN(mime, "/", 2); len(parts) == 2 && parts[1] != "" {
		return parts[1]
	}
	return fallback
}

func fileExt(fileName, fallback string) string {
	if idx := strings.LastIndexByte(fileName, '.'); idx >= 0 {
		return fileName[idx+1:]
	}
	return fallback
}
