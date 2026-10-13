package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"gopkg.in/yaml.v3"
)

// ResponseAction описывает одно действие ответа
type ResponseAction struct {
	Type     string  `yaml:"type"`
	Content  string  `yaml:"content,omitempty"`
	URL      string  `yaml:"url,omitempty"`
	Path     string  `yaml:"path,omitempty"`
	Duration float64 `yaml:"duration,omitempty"`
}

// PatternConfig описывает отдельный паттерн и его переопределения
type PatternConfig struct {
	Pattern      string           `yaml:"pattern"`
	MuteDuration time.Duration    `yaml:"mute_duration"`
	Responses    []ResponseAction `yaml:"responses"`
}

// CompiledPattern связывает скомпилированную регулярку с ее настройками
type CompiledPattern struct {
	Regexp *regexp.Regexp
	Config PatternConfig
}

type Config struct {
	BotToken          string           `yaml:"bot_token"`
	GuildID           string           `yaml:"guild_id"`
	MonitoredChannels []string         `yaml:"monitored_channels"`
	MuteRoleID        string           `yaml:"mute_role_id"`
	IgnoreReplies     bool             `yaml:"ignore_replies"`
	StateFilePath     string           `yaml:"state_file_path"`
	MuteDuration      time.Duration    `yaml:"mute_duration"` // Дефолтное время
	Responses         []ResponseAction `yaml:"responses"`     // Дефолтные ответы
	Patterns          []PatternConfig  `yaml:"patterns"`
}

type MuteRecord struct {
	GuildID  string    `json:"guild_id"`
	UserID   string    `json:"user_id"`
	UnmuteAt time.Time `json:"unmute_at"`
}

type Bot struct {
	config     Config
	session    *discordgo.Session
	patterns   []CompiledPattern // Храним скомпилированные паттерны с их конфигами
	state      map[string]MuteRecord
	stateMutex sync.Mutex
}

func main() {
	cfgBytes, err := os.ReadFile("config.yaml")
	if err != nil {
		log.Fatalf("Ошибка чтения config.yaml: %v", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(cfgBytes, &cfg); err != nil {
		log.Fatalf("Ошибка парсинга YAML: %v", err)
	}

	// Компилируем регулярки и привязываем к ним их настройки
	var compiledPatterns []CompiledPattern
	for _, p := range cfg.Patterns {
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			log.Fatalf("Ошибка компиляции паттерна '%s': %v", p.Pattern, err)
		}
		compiledPatterns = append(compiledPatterns, CompiledPattern{
			Regexp: re,
			Config: p,
		})
	}

	dg, err := discordgo.New("Bot " + cfg.BotToken)
	if err != nil {
		log.Fatalf("Ошибка создания сессии Discord: %v", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent | discordgo.IntentsGuilds

	b := &Bot{
		config:   cfg,
		session:  dg,
		patterns: compiledPatterns,
		state:    make(map[string]MuteRecord),
	}

	b.loadState()
	dg.AddHandler(b.messageCreate)

	if err = dg.Open(); err != nil {
		log.Fatalf("Ошибка подключения к Discord: %v", err)
	}
	defer dg.Close()

	log.Println("Бот успешно запущен! Ждем нарушителей...")
	b.processLoadedMutes()

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
	log.Println("Завершение работы бота...")
}

func (b *Bot) messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID || !contains(b.config.MonitoredChannels, m.ChannelID) {
		return
	}
	if b.config.IgnoreReplies && m.MessageReference != nil {
		return
	}
	if m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply {
		return
	}

	textToCheck := m.Content
	for _, att := range m.Attachments {
		textToCheck += " " + att.Filename
	}

	// Ищем совпадение
	var matchedPattern *CompiledPattern
	for _, cp := range b.patterns {
		if cp.Regexp.MatchString(textToCheck) {
			matchedPattern = &cp
			break // Останавливаемся на первом совпавшем паттерне
		}
	}

	if matchedPattern == nil {
		return // Совпадений нет
	}

	log.Printf("Обнаружен триггер ('%s') от %s (%s)", matchedPattern.Config.Pattern, m.Author.Username, m.Author.ID)

	// --- ОПРЕДЕЛЯЕМ НАСТРОЙКИ (Специфичные или Дефолтные) ---
	
	// 1. Длительность мута
	duration := b.config.MuteDuration
	if matchedPattern.Config.MuteDuration != 0 {
		duration = matchedPattern.Config.MuteDuration
	}

	// 2. Действия ответа (Responses)
	actions := b.config.Responses
	if len(matchedPattern.Config.Responses) > 0 {
		actions = matchedPattern.Config.Responses
	}

	// Выдаем роль
	err := s.GuildMemberRoleAdd(b.config.GuildID, m.Author.ID, b.config.MuteRoleID)
	if err != nil {
		log.Printf("❌ ОШИБКА выдачи роли мута: %v", err)
	} else {
		log.Printf("✅ Выдан мут пользователю %s на %v", m.Author.Username, duration)
	}

	// Планируем снятие мута с учетом вычисленной duration
	unmuteTime := time.Now().Add(duration)
	b.saveMute(b.config.GuildID, m.Author.ID, unmuteTime)
	b.scheduleUnmute(b.config.GuildID, m.Author.ID, duration)

	// Отправляем ответ, передавая вычисленный массив actions
	b.sendReply(m.Message, actions)
}

// Теперь функция принимает конкретный список действий
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

// Вспомогательные методы отправки и работы со стейтом остаются без изменений
func (b *Bot) sendTextMsg(channelID, messageID, content string) {
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

func (b *Bot) scheduleUnmute(guildID, userID string, duration time.Duration) {
	time.AfterFunc(duration, func() {
		b.stateMutex.Lock()
		_, exists := b.state[userID]
		b.stateMutex.Unlock()

		if !exists {
			return
		}

		_ = b.session.GuildMemberRoleRemove(guildID, userID, b.config.MuteRoleID)
		b.removeMute(userID)
		log.Printf("Мут снят с %s", userID)
	})
}

func (b *Bot) processLoadedMutes() {
	b.stateMutex.Lock()
	defer b.stateMutex.Unlock()
	now := time.Now()
	for userID, record := range b.state {
		if now.After(record.UnmuteAt) {
			_ = b.session.GuildMemberRoleRemove(record.GuildID, record.UserID, b.config.MuteRoleID)
			delete(b.state, userID)
			log.Printf("Снят просроченный мут с %s", userID)
		} else {
			durationLeft := record.UnmuteAt.Sub(now)
			go b.scheduleUnmute(record.GuildID, record.UserID, durationLeft)
		}
	}
	b.persistState()
}

func (b *Bot) saveMute(guildID, userID string, unmuteAt time.Time) {
	b.stateMutex.Lock()
	defer b.stateMutex.Unlock()
	b.state[userID] = MuteRecord{GuildID: guildID, UserID: userID, UnmuteAt: unmuteAt}
	b.persistState()
}

func (b *Bot) removeMute(userID string) {
	b.stateMutex.Lock()
	defer b.stateMutex.Unlock()
	delete(b.state, userID)
	b.persistState()
}

func (b *Bot) persistState() {
	data, _ := json.MarshalIndent(b.state, "", "  ")
	_ = os.WriteFile(b.config.StateFilePath, data, 0644)
}

func (b *Bot) loadState() {
	data, err := os.ReadFile(b.config.StateFilePath)
	if err == nil {
		b.stateMutex.Lock()
		defer b.stateMutex.Unlock()
		_ = json.Unmarshal(data, &b.state)
	}
}

func contains(slice []string, val string) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}
