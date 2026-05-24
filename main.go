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
	"regexp"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gopkg.in/yaml.v3"
)

type Config struct {
	TelegramToken     string    `yaml:"telegram_token"`
	Channels          []Channel `yaml:"channels"`
	RepoPath          string    `yaml:"repo_path"`
	GitBranch         string    `yaml:"git_branch"`
	GitRemote         string    `yaml:"git_remote"`
	GitAuthorName     string    `yaml:"git_author_name"`
	GitAuthorEmail    string    `yaml:"git_author_email"`
	MaxFileSizeMB     float64   `yaml:"max_file_size_mb"`
	CommitDelayMin    float64   `yaml:"commit_delay_min"`
	CommitMaxDelayMin float64   `yaml:"commit_max_delay_min"`
	Push              bool      `yaml:"push"`
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
	syncMu     sync.Mutex // serialises gitSync calls
	timer      *time.Timer
	hasDirty   bool
	firstDirty time.Time
	known      map[int64]string // chat ID -> folder name
}

// editTagRe matches " edit:HH:MM:SS" inside a message header.
var editTagRe = regexp.MustCompile(` edit:\d{2}:\d{2}:\d{2}`)

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

	b := &Bot{cfg: &cfg, api: api, known: make(map[int64]string)}
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
			b.handleNew(update.Message)
		case update.ChannelPost != nil:
			b.handleNew(update.ChannelPost)
		case update.EditedMessage != nil:
			b.handleEdit(update.EditedMessage)
		case update.EditedChannelPost != nil:
			b.handleEdit(update.EditedChannelPost)
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
	// No channels configured → accept all, derive name from chat metadata.
	if len(b.cfg.Channels) == 0 {
		log.Printf("chat id=%d title=%q username=%q", chat.ID, chat.Title, chat.UserName)
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

// handleNew records a new incoming message.
//
// Line format:
//
//	[YYYY-MM-DD HH:MM:SS #ID] @sender: content
func (b *Bot) handleNew(msg *tgbotapi.Message) {
	folder, ok := b.folderName(msg.Chat)
	if !ok {
		return
	}

	ts := time.Unix(int64(msg.Date), 0).UTC()
	year, date, tsStr := ts.Format("2006"), ts.Format("2006-01-02"), ts.Format("2006-01-02 15:04:05")

	content := b.contentFromMessage(msg, folder, year, date)
	if content == "" {
		return
	}

	dirPath := filepath.Join(b.cfg.RepoPath, "telegram", folder, year)
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		log.Printf("mkdir %s: %v", dirPath, err)
		return
	}

	line := fmt.Sprintf("[%s #%d] %s: %s", tsStr, msg.MessageID, senderStr(msg.From), content)
	txtFile := filepath.Join(dirPath, date+".txt")
	f, err := os.OpenFile(txtFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("open %s: %v", txtFile, err)
		return
	}
	_, werr := fmt.Fprintln(f, line)
	f.Close()
	if werr != nil {
		log.Printf("write: %v", werr)
		return
	}

	if err := b.indexAdd(folder, msg.MessageID, date); err != nil {
		log.Printf("index add: %v", err)
	}
	b.scheduleCommit()
}

// handleEdit updates an existing message line in-place.
// The header gains an "edit:HH:MM:SS" tag; the content is replaced with the new text.
// Full change history is available via git log -p.
func (b *Bot) handleEdit(msg *tgbotapi.Message) {
	folder, ok := b.folderName(msg.Chat)
	if !ok {
		return
	}

	editTs := time.Unix(int64(msg.EditDate), 0).UTC()
	editTime := editTs.Format("15:04:05")

	// Find the file that holds the original message (may be a different day).
	origDate, found := b.indexLookup(folder, msg.MessageID)
	if !found {
		// Fallback: assume same day as the message timestamp.
		origDate = time.Unix(int64(msg.Date), 0).UTC().Format("2006-01-02")
	}
	year := origDate[:4]

	content := b.contentFromMessage(msg, folder, year, origDate)
	if content == "" {
		return
	}

	txtFile := filepath.Join(b.cfg.RepoPath, "telegram", folder, year, origDate+".txt")
	idTag := fmt.Sprintf(" #%d]", msg.MessageID)

	updated, err := replaceLine(txtFile,
		func(line string) bool { return strings.Contains(line, idTag) },
		func(line string) string {
			// Line: "[TIMESTAMP #ID(optional edit:X)] sender: old_content"
			closeBracket := strings.Index(line, "]")
			if closeBracket < 0 {
				return line
			}
			header := line[1:closeBracket] // between "[" and "]"
			rest := ""
			if len(line) > closeBracket+2 {
				rest = line[closeBracket+2:] // after "] "
			}
			colonIdx := strings.Index(rest, ": ")
			if colonIdx < 0 {
				return line
			}
			sender := rest[:colonIdx]

			// Replace any previous edit tag, append new one.
			cleanHeader := editTagRe.ReplaceAllString(header, "")
			return "[" + cleanHeader + " edit:" + editTime + "] " + sender + ": " + content
		},
	)
	if err != nil {
		log.Printf("update line in %s: %v", txtFile, err)
		return
	}
	if !updated {
		log.Printf("message #%d not found in %s", msg.MessageID, txtFile)
	}
	b.scheduleCommit()
}

// contentFromMessage returns a single-line text representation of the message content.
// Newlines inside text are escaped as \n.
func (b *Bot) contentFromMessage(msg *tgbotapi.Message, folder, year, date string) string {
	switch {
	case msg.Text != "":
		return escNL(msg.Text)
	case msg.Photo != nil:
		photo := msg.Photo[len(msg.Photo)-1]
		media := b.saveMedia(folder, year, date, "Photo", "jpg", photo.FileID, int64(photo.FileSize))
		return media + captionSuffix(escNL(msg.Caption))
	case msg.Video != nil:
		v := msg.Video
		media := b.saveMedia(folder, year, date, "Video", extFromMime(v.MimeType, "mp4"), v.FileID, int64(v.FileSize))
		return media + captionSuffix(escNL(msg.Caption))
	case msg.Document != nil:
		d := msg.Document
		ext := fileExt(d.FileName, extFromMime(d.MimeType, "bin"))
		media := b.saveMedia(folder, year, date, "Document", ext, d.FileID, int64(d.FileSize))
		return media + captionSuffix(escNL(msg.Caption))
	case msg.Audio != nil:
		a := msg.Audio
		return b.saveMedia(folder, year, date, "Audio", extFromMime(a.MimeType, "mp3"), a.FileID, int64(a.FileSize))
	case msg.Voice != nil:
		return b.saveMedia(folder, year, date, "Voice", "ogg", msg.Voice.FileID, int64(msg.Voice.FileSize))
	case msg.VideoNote != nil:
		return b.saveMedia(folder, year, date, "VideoNote", "mp4", msg.VideoNote.FileID, int64(msg.VideoNote.FileSize))
	case msg.Sticker != nil:
		s := msg.Sticker
		emoji := ""
		if s.Emoji != "" {
			emoji = s.Emoji + " "
		}
		return emoji + b.saveMedia(folder, year, date, "Sticker", "webp", s.FileID, int64(s.FileSize))
	}
	return ""
}

// replaceLine reads a file, transforms the first line matching match(), and writes it back.
func replaceLine(path string, match func(string) bool, transform func(string) string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if match(line) {
			lines[i] = transform(line)
			return true, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
		}
	}
	return false, nil
}

// Index file: telegram/{folder}/.msgindex
// Format: one line per message — "{msgID} {date}" (e.g. "12345 2025-05-24")
// Used to find the day-file for cross-day edits.

func (b *Bot) indexPath(folder string) string {
	return filepath.Join(b.cfg.RepoPath, "telegram", folder, ".msgindex")
}

func (b *Bot) indexAdd(folder string, msgID int, date string) error {
	dir := filepath.Join(b.cfg.RepoPath, "telegram", folder)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(b.indexPath(folder), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%d %s\n", msgID, date)
	return err
}

func (b *Bot) indexLookup(folder string, msgID int) (string, bool) {
	data, err := os.ReadFile(b.indexPath(folder))
	if err != nil {
		return "", false
	}
	prefix := fmt.Sprintf("%d ", msgID)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix), true
		}
	}
	return "", false
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

	// Idempotent: skip download if the file already exists (handles re-edits).
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

// scheduleCommit arranges a git commit+push with two-tier debounce:
//   - delay timer resets on each new event (CommitDelayMin)
//   - hard deadline from first pending change (CommitMaxDelayMin)
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

func (b *Bot) gitSync() {
	dir := b.cfg.RepoPath
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME="+b.cfg.GitAuthorName,
		"GIT_AUTHOR_EMAIL="+b.cfg.GitAuthorEmail,
		"GIT_COMMITTER_NAME="+b.cfg.GitAuthorName,
		"GIT_COMMITTER_EMAIL="+b.cfg.GitAuthorEmail,
	)

	execGit := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	run := func(args ...string) (string, error) {
		out, err := execGit(args...)
		if err != nil {
			log.Printf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(out))
		}
		return out, err
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

	if _, err := execGit("push", b.cfg.GitRemote, b.cfg.GitBranch); err == nil {
		log.Println("pushed successfully")
		return
	}
	log.Println("push rejected, pulling with rebase...")
	if _, err := run("pull", "--rebase", b.cfg.GitRemote, b.cfg.GitBranch); err != nil {
		run("rebase", "--abort") //nolint:errcheck
		log.Println("rebase failed, force pushing")
		if _, err := run("push", "--force", b.cfg.GitRemote, b.cfg.GitBranch); err == nil {
			log.Println("pushed successfully")
		}
		return
	}
	if _, err := execGit("push", b.cfg.GitRemote, b.cfg.GitBranch); err != nil {
		log.Println("push failed after rebase, force pushing")
		if _, err := run("push", "--force", b.cfg.GitRemote, b.cfg.GitBranch); err == nil {
			log.Println("pushed successfully")
		}
		return
	}
	log.Println("pushed successfully")
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

func escNL(s string) string {
	return strings.ReplaceAll(s, "\n", `\n`)
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
