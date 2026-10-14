package main

import (
	"bytes"
	crand "crypto/rand" // Алиас, чтобы избежать конфликта с math/rand
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2" // Основной rand для генерации чисел
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	_ "embed" // Для встраивания HTML файла

	"github.com/bwmarrin/discordgo"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

//go:embed index.html
var indexHTML []byte

type ResponseAction struct {
	Type     string  `yaml:"type" json:"type"`
	Content  string  `yaml:"content,omitempty" json:"content,omitempty"`
	URL      string  `yaml:"url,omitempty" json:"url,omitempty"`
	Path     string  `yaml:"path,omitempty" json:"path,omitempty"`
	Duration float64 `yaml:"duration,omitempty" json:"duration,omitempty"`
}

type AntiSpamConfig struct {
	Enabled      bool             `yaml:"enabled" json:"enabled"`
	ThresholdMin int              `yaml:"threshold_min" json:"threshold_min"`
	ThresholdMax int              `yaml:"threshold_max" json:"threshold_max"`
	ResetTime    string           `yaml:"reset_time" json:"reset_time"`
	MuteDuration string           `yaml:"mute_duration" json:"mute_duration"`
	Responses    []ResponseAction `yaml:"responses" json:"responses"`
}

type PatternConfig struct {
	Description  string           `yaml:"description,omitempty" json:"description"`
	Pattern      string           `yaml:"pattern" json:"pattern"`
	Enabled      bool             `yaml:"enabled" json:"enabled"`
	DisableMute  bool             `yaml:"disable_mute" json:"disable_mute"`
	ReactChance  float64          `yaml:"react_chance" json:"react_chance"`
	MuteDuration string           `yaml:"mute_duration,omitempty" json:"mute_duration,omitempty"`
	Responses    []ResponseAction `yaml:"responses,omitempty" json:"responses,omitempty"`
}

type Config struct {
	BotToken          string           `yaml:"bot_token" json:"-"`
	GuildID           string           `yaml:"guild_id" json:"-"`
	StateFilePath     string           `yaml:"state_file_path" json:"-"`
	WebPort           int              `yaml:"web_port" json:"-"`
	MonitoredChannels []string         `yaml:"monitored_channels" json:"monitored_channels"`
	MuteRoleID        string           `yaml:"mute_role_id" json:"mute_role_id"`
	IgnoreReplies     bool             `yaml:"ignore_replies" json:"ignore_replies"`
	MuteDuration      string           `yaml:"mute_duration" json:"mute_duration"`
	AntiSpam          AntiSpamConfig   `yaml:"anti_spam" json:"anti_spam"`
	Responses         []ResponseAction `yaml:"responses" json:"responses"`
	Patterns          []PatternConfig  `yaml:"patterns" json:"patterns"`
}

type CompiledPattern struct {
	Regexp *regexp.Regexp
	Config PatternConfig
}

type SpamState struct {
	Count      int
	Threshold  int
	LastAction time.Time
}

type Bot struct {
	config        Config
	compiledPatts []CompiledPattern
	cfgMutex      sync.RWMutex

	session *discordgo.Session

	mutes      map[string]time.Time
	mutesMutex sync.Mutex

	spam      map[string]*SpamState
	spamMutex sync.Mutex

	sessions map[string]string
	users    map[string]string

	auditLogger *log.Logger
}

func main() {
	b := &Bot{
		mutes:    make(map[string]time.Time),
		spam:     make(map[string]*SpamState),
		sessions: make(map[string]string),
		users:    make(map[string]string),
	}

	b.initAuditLogger()
	b.loadUsers()
	b.loadConfig("config.yaml")

	dg, err := discordgo.New("Bot " + b.config.BotToken)
	if err != nil {
		log.Fatalf("Ошибка Discord: %v", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent | discordgo.IntentsGuilds
	b.session = dg

	b.loadMutes()
	dg.AddHandler(b.messageCreate)

	if err = dg.Open(); err != nil {
		log.Fatalf("Ошибка подключения к Discord: %v", err)
	}
	defer dg.Close()
	log.Println("🤖 Бот успешно запущен!")

	b.processLoadedMutes()

	go b.startWebServer()

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
	log.Println("Завершение работы...")
}

// ====================== WEB SERVER & API ======================

func (b *Bot) startWebServer() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	mux.HandleFunc("POST /api/login", b.handleLogin)
	mux.HandleFunc("POST /api/logout", b.handleLogout)

	mux.Handle("/api/config", b.authMiddleware(http.HandlerFunc(b.handleConfig)))
	mux.Handle("GET /api/config/export", b.authMiddleware(http.HandlerFunc(b.handleConfigExport)))

	port := fmt.Sprintf(":%d", b.config.WebPort)
	log.Printf("🌐 Web-интерфейс запущен на http://localhost%s", port)
	if err := http.ListenAndServe(port, mux); err != nil {
		log.Fatalf("Ошибка Web-сервера: %v", err)
	}
}

func (b *Bot) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session_token")
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		b.cfgMutex.RLock()
		user, exists := b.sessions[cookie.Value]
		b.cfgMutex.RUnlock()

		if !exists {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		r.Header.Set("X-User", user)
		next.ServeHTTP(w, r)
	})
}

func (b *Bot) handleLogin(w http.ResponseWriter, r *http.Request) {
	var creds struct{ Username, Password string }
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	b.cfgMutex.RLock()
	hash, exists := b.users[creds.Username]
	b.cfgMutex.RUnlock()

	if !exists || bcrypt.CompareHashAndPassword([]byte(hash), []byte(creds.Password)) != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	sessionToken := generateToken()
	b.cfgMutex.Lock()
	b.sessions[sessionToken] = creds.Username
	b.cfgMutex.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    sessionToken,
		Expires:  time.Now().Add(24 * time.Hour),
		HttpOnly: true,
	})

	b.auditLog(creds.Username, "Успешный вход в систему")
	w.WriteHeader(http.StatusOK)
}

func (b *Bot) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_token")
	if err == nil {
		b.cfgMutex.Lock()
		delete(b.sessions, cookie.Value)
		b.cfgMutex.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "session_token", Value: "", Expires: time.Now().Add(-1 * time.Hour)})
	w.WriteHeader(http.StatusOK)
}

func (b *Bot) handleConfig(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get("X-User")

	if r.Method == "GET" {
		b.cfgMutex.RLock()
		defer b.cfgMutex.RUnlock()
		json.NewEncoder(w).Encode(b.config)
		return
	}

	if r.Method == "POST" {
		var newPartialConfig Config
		if err := json.NewDecoder(r.Body).Decode(&newPartialConfig); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		b.cfgMutex.Lock()
		newPartialConfig.BotToken = b.config.BotToken
		newPartialConfig.GuildID = b.config.GuildID
		newPartialConfig.StateFilePath = b.config.StateFilePath
		newPartialConfig.WebPort = b.config.WebPort

		b.config = newPartialConfig
		b.compilePatternsUnsafe()
		b.cfgMutex.Unlock()

		b.saveConfigToDisk()
		b.auditLog(user, "Конфигурация была изменена")

		w.WriteHeader(http.StatusOK)
	}
}

func (b *Bot) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	b.cfgMutex.RLock()
	defer b.cfgMutex.RUnlock()
	w.Header().Set("Content-Disposition", "attachment; filename=config_safe.yaml")
	w.Header().Set("Content-Type", "application/x-yaml")

	type SafeExport struct {
		MonitoredChannels []string         `yaml:"monitored_channels"`
		MuteRoleID        string           `yaml:"mute_role_id"`
		IgnoreReplies     bool             `yaml:"ignore_replies"`
		MuteDuration      string           `yaml:"mute_duration"`
		AntiSpam          AntiSpamConfig   `yaml:"anti_spam"`
		Responses         []ResponseAction `yaml:"responses"`
		Patterns          []PatternConfig  `yaml:"patterns"`
	}

	exportData := SafeExport{
		MonitoredChannels: b.config.MonitoredChannels,
		MuteRoleID:        b.config.MuteRoleID,
		IgnoreReplies:     b.config.IgnoreReplies,
		MuteDuration:      b.config.MuteDuration,
		AntiSpam:          b.config.AntiSpam,
		Responses:         b.config.Responses,
		Patterns:          b.config.Patterns,
	}

	yaml.NewEncoder(w).Encode(exportData)
	b.auditLog(r.Header.Get("X-User"), "Выгружен безопасный конфиг")
}

// ====================== DISCORD LOGIC ======================

func (b *Bot) messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	b.cfgMutex.RLock()
	cfg := b.config
	patts := b.compiledPatts
	b.cfgMutex.RUnlock()

	if m.Author.ID == s.State.User.ID || !contains(cfg.MonitoredChannels, m.ChannelID) {
		return
	}

	if cfg.AntiSpam.Enabled && b.isTargetingBot(m) {
		if b.handleAntiSpam(m.Author.ID, cfg) {
			duration, _ := time.ParseDuration(cfg.AntiSpam.MuteDuration)
			b.applyPunishment(m.Author.ID, m.Author.Username, duration, cfg.GuildID, cfg.MuteRoleID, false)
			b.sendReply(m.Message, cfg.AntiSpam.Responses)
			return
		}
	}

	if cfg.IgnoreReplies && m.MessageReference != nil {
		return
	}
	if m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply {
		return
	}

	textToCheck := m.Content
	for _, att := range m.Attachments {
		textToCheck += " " + att.Filename
	}

	var matched *CompiledPattern
	for _, cp := range patts {
		if cp.Config.Enabled && cp.Regexp.MatchString(textToCheck) {
			matched = &cp
			break
		}
	}

	if matched == nil {
		return
	}

	if matched.Config.ReactChance < 1.0 {
		if rand.Float64() > matched.Config.ReactChance {
			log.Printf("Паттерн '%s' проигнорирован из-за шанса (ReactChance=%.2f)", matched.Config.Pattern, matched.Config.ReactChance)
			return
		}
	}

	log.Printf("Обнаружен триггер ('%s') от %s", matched.Config.Pattern, m.Author.Username)

	durStr := cfg.MuteDuration
	if matched.Config.MuteDuration != "" {
		durStr = matched.Config.MuteDuration
	}
	duration, _ := time.ParseDuration(durStr)

	actions := cfg.Responses
	if len(matched.Config.Responses) > 0 {
		actions = matched.Config.Responses
	}

	b.applyPunishment(m.Author.ID, m.Author.Username, duration, cfg.GuildID, cfg.MuteRoleID, matched.Config.DisableMute)
	b.sendReply(m.Message, actions)
}

func (b *Bot) isTargetingBot(m *discordgo.MessageCreate) bool {
	for _, u := range m.Mentions {
		if u.ID == b.session.State.User.ID {
			return true
		}
	}
	if m.MessageReference != nil {
		refMsg, err := b.session.State.Message(m.ChannelID, m.MessageReference.MessageID)
		if err == nil && refMsg.Author.ID == b.session.State.User.ID {
			return true
		}
	}
	return false
}

func (b *Bot) handleAntiSpam(userID string, cfg Config) bool {
	b.spamMutex.Lock()
	defer b.spamMutex.Unlock()

	state, exists := b.spam[userID]
	resetDur, _ := time.ParseDuration(cfg.AntiSpam.ResetTime)

	if !exists || time.Since(state.LastAction) > resetDur {
		threshold := cfg.AntiSpam.ThresholdMin
		if cfg.AntiSpam.ThresholdMax > cfg.AntiSpam.ThresholdMin {
			threshold = rand.IntN(cfg.AntiSpam.ThresholdMax-cfg.AntiSpam.ThresholdMin+1) + cfg.AntiSpam.ThresholdMin
		}
		state = &SpamState{Count: 0, Threshold: threshold}
		b.spam[userID] = state
	}

	state.Count++
	state.LastAction = time.Now()

	if state.Count >= state.Threshold {
		delete(b.spam, userID)
		return true
	}
	return false
}

func (b *Bot) applyPunishment(userID, username string, duration time.Duration, guildID, roleID string, disableMute bool) {
	if disableMute {
		log.Printf("Мут отменен параметром disable_mute для %s", username)
		return
	}

	if err := b.session.GuildMemberRoleAdd(guildID, userID, roleID); err != nil {
		log.Printf("❌ Ошибка выдачи мута: %v", err)
		return
	}
	log.Printf("✅ Выдан мут пользователю %s на %v", username, duration)

	unmuteAt := time.Now().Add(duration)
	b.mutesMutex.Lock()
	b.mutes[userID] = unmuteAt
	b.mutesMutex.Unlock()

	b.persistMutes()
	b.scheduleUnmute(guildID, userID, roleID, duration)
}

func (b *Bot) sendReply(m *discordgo.Message, actions []ResponseAction) {
	for _, action := range actions {
		switch action.Type {
		case "text":
			b.sendTextMsg(m.ChannelID, m.ID, action.Content)
		case "gif":
			b.sendTextMsg(m.ChannelID, m.ID, action.URL)
		case "voice":
			err := b.sendActualVoiceMessage(m.ChannelID, m.ID, action.Path, action.Duration)
			if err != nil {
				log.Printf("Ошибка отправки voice: %v", err)
			}
		default:
			log.Printf("Неизвестный тип ответа: %s", action.Type)
		}
		time.Sleep(700 * time.Millisecond)
	}
}

func (b *Bot) sendTextMsg(channelID, messageID, content string) {
	if content == "" {
		return
	}
	_, err := b.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: content,
		Reference: &discordgo.MessageReference{
			MessageID: messageID,
			ChannelID: channelID,
		},
	})
	if err != nil {
		log.Printf("Ошибка отправки текста/гифки: %v", err)
	}
}

func (b *Bot) sendActualVoiceMessage(channelID, replyToID, filePath string, duration float64) error {
	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("ошибка чтения аудио файла: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	apiBase := fmt.Sprintf("https://discord.com/api/v10/channels/%s", channelID)

	attReq := map[string]any{
		"files": []map[string]any{{"filename": "voice.ogg", "file_size": len(fileData), "id": "0"}},
	}
	attBytes, _ := json.Marshal(attReq)

	req, _ := http.NewRequest("POST", apiBase+"/attachments", bytes.NewReader(attBytes))
	req.Header.Set("Authorization", "Bot "+b.config.BotToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("ошибка получения URL (HTTP %d)", resp.StatusCode)
	}

	var attResp struct {
		Attachments []struct {
			UploadURL      string `json:"upload_url"`
			UploadFilename string `json:"upload_filename"`
		} `json:"attachments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&attResp); err != nil || len(attResp.Attachments) == 0 {
		return fmt.Errorf("некорректный ответ API при запросе вложений")
	}

	putReq, _ := http.NewRequest("PUT", attResp.Attachments[0].UploadURL, bytes.NewReader(fileData))
	putReq.Header.Set("Content-Type", "audio/ogg")
	putResp, err := client.Do(putReq)
	if err != nil {
		return err
	}
	defer putResp.Body.Close()
	if putResp.StatusCode >= 300 {
		return fmt.Errorf("ошибка загрузки файла на сервер Discord (HTTP %d)", putResp.StatusCode)
	}

	waveformBytes := make([]byte, 256)
	for i := range waveformBytes {
		waveformBytes[i] = byte(math.Abs(math.Sin(float64(i)*0.1)) * 128)
	}
	waveformBase64 := base64.StdEncoding.EncodeToString(waveformBytes)

	msgReq := map[string]any{
		"flags": 8192,
		"attachments": []map[string]any{{
			"id":                "0",
			"filename":          "voice.ogg",
			"uploaded_filename": attResp.Attachments[0].UploadFilename,
			"duration_secs":     duration,
			"waveform":          waveformBase64,
		}},
		"message_reference": map[string]any{"message_id": replyToID},
	}
	msgBytes, _ := json.Marshal(msgReq)

	msgHTTPReq, _ := http.NewRequest("POST", apiBase+"/messages", bytes.NewReader(msgBytes))
	msgHTTPReq.Header.Set("Authorization", "Bot "+b.config.BotToken)
	msgHTTPReq.Header.Set("Content-Type", "application/json")

	msgResp, err := client.Do(msgHTTPReq)
	if err != nil {
		return err
	}
	defer msgResp.Body.Close()

	if msgResp.StatusCode >= 300 {
		body, _ := io.ReadAll(msgResp.Body)
		return fmt.Errorf("ошибка отправки Voice Message (HTTP %d): %s", msgResp.StatusCode, string(body))
	}
	return nil
}

// ====================== UTILS & SETUP ======================

func (b *Bot) compilePatternsUnsafe() {
	b.compiledPatts = nil
	for _, p := range b.config.Patterns {
		re, err := regexp.Compile(p.Pattern)
		if err == nil {
			b.compiledPatts = append(b.compiledPatts, CompiledPattern{Regexp: re, Config: p})
		}
	}
}

func (b *Bot) saveConfigToDisk() {
	b.cfgMutex.RLock()
	data, _ := yaml.Marshal(b.config)
	b.cfgMutex.RUnlock()
	os.WriteFile("config.yaml", data, 0644)
}

func (b *Bot) loadConfig(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Ошибка чтения конфига: %v", err)
	}
	b.cfgMutex.Lock()
	yaml.Unmarshal(data, &b.config)

	if b.config.WebPort == 0 {
		b.config.WebPort = 8080
	}
	if b.config.AntiSpam.ThresholdMin == 0 {
		b.config.AntiSpam.ThresholdMin = 3
	}
	if b.config.AntiSpam.ThresholdMax == 0 {
		b.config.AntiSpam.ThresholdMax = 7
	}
	if b.config.AntiSpam.ResetTime == "" {
		b.config.AntiSpam.ResetTime = "30m"
	}

	b.compilePatternsUnsafe()
	b.cfgMutex.Unlock()
}

func (b *Bot) initAuditLogger() {
	os.MkdirAll("data", 0755)
	file, err := os.OpenFile("data/audit.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		log.Fatal(err)
	}
	b.auditLogger = log.New(file, "", log.LstdFlags)
}

func (b *Bot) auditLog(user, action string) {
	b.auditLogger.Printf("[User: %s] %s\n", user, action)
}

func (b *Bot) loadUsers() {
	data, err := os.ReadFile("data/users.json")
	if err == nil {
		json.Unmarshal(data, &b.users)
	}
	if len(b.users) == 0 {
		log.Println("⚠️ Создан дефолтный пользователь admin:admin (Смените пароль!)")
		hash, _ := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
		b.users["admin"] = string(hash)
		os.MkdirAll("data", 0755)
		d, _ := json.Marshal(b.users)
		os.WriteFile("data/users.json", d, 0644)
	}
}

func generateToken() string {
	b := make([]byte, 16)
	crand.Read(b) // Теперь используем правильный rand (crypto/rand)
	return hex.EncodeToString(b)
}

func contains(slice []string, val string) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}

func (b *Bot) scheduleUnmute(guildID, userID, roleID string, duration time.Duration) {
	time.AfterFunc(duration, func() {
		b.mutesMutex.Lock()
		_, exists := b.mutes[userID]
		if exists {
			delete(b.mutes, userID)
		}
		b.mutesMutex.Unlock()
		if exists {
			_ = b.session.GuildMemberRoleRemove(guildID, userID, roleID)
			b.persistMutes()
			log.Printf("Мут снят с %s", userID)
		}
	})
}

func (b *Bot) persistMutes() {
	b.mutesMutex.Lock()
	data, _ := json.Marshal(b.mutes)
	b.mutesMutex.Unlock()
	b.cfgMutex.RLock()
	path := b.config.StateFilePath
	b.cfgMutex.RUnlock()
	os.WriteFile(path, data, 0644)
}

func (b *Bot) loadMutes() {
	b.cfgMutex.RLock()
	path := b.config.StateFilePath
	b.cfgMutex.RUnlock()
	data, err := os.ReadFile(path)
	if err == nil {
		b.mutesMutex.Lock()
		json.Unmarshal(data, &b.mutes)
		b.mutesMutex.Unlock()
	}
}

func (b *Bot) processLoadedMutes() {
	b.mutesMutex.Lock()
	defer b.mutesMutex.Unlock()
	b.cfgMutex.RLock()
	gID := b.config.GuildID
	rID := b.config.MuteRoleID
	b.cfgMutex.RUnlock()

	now := time.Now()
	for userID, unmuteAt := range b.mutes {
		if now.After(unmuteAt) {
			_ = b.session.GuildMemberRoleRemove(gID, userID, rID)
			delete(b.mutes, userID)
		} else {
			go b.scheduleUnmute(gID, userID, rID, unmuteAt.Sub(now))
		}
	}
}
