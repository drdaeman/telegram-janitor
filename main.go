package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/postgres"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
	log "github.com/sirupsen/logrus"
	"github.com/xo/dburl"
	"github.com/zpatrick/go-config"
	tb "gopkg.in/tucnak/telebot.v2"
)

var release string

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// Message is a GORM model struct that contains Telegram message IDs.
type Message struct {
	ID        uint `gorm:"primary_key"`
	MessageID int
	ChatID    int64
	Time      time.Time
	Type      string
	Deleted   bool
}

// MessageSig returns a message IDs, conformant to Telebot's Editable interface.
func (msg *Message) MessageSig() (messageID string, chatID int64) {
	return strconv.Itoa(msg.MessageID), msg.ChatID
}

// Bot is a structure with all bot's configuration and dependencies.
type Bot struct {
	tg            *tb.Bot
	db            *gorm.DB
	dbDriver      string
	MessageTTL    time.Duration
	SweepInterval time.Duration
}

// NewBot creates a Bot from given Config. It does not start the bot.
func NewBot(cfg *config.Config) (*Bot, error) {
	bot := new(Bot)

	token, err := cfg.String("janitor.token")
	if err != nil {
		return nil, err
	}
	dbURIText, err := cfg.StringOr("janitor.database", "sqlite:janitor.sqlite3")
	if err != nil {
		return nil, err
	}
	dbURL, err := dburl.Parse(dbURIText)
	if err != nil {
		return nil, err
	}
	ttlText, err := cfg.StringOr("janitor.ttl", "22h")
	if err != nil {
		return nil, err
	}
	bot.MessageTTL, err = time.ParseDuration(ttlText)
	if err != nil {
		return nil, err
	}
	sweepIntervalText, err := cfg.StringOr("janitor.interval", "1m")
	if err != nil {
		return nil, err
	}
	bot.SweepInterval, err = time.ParseDuration(sweepIntervalText)
	if err != nil {
		return nil, err
	}

	bot.dbDriver = dbURL.Driver
	bot.db, err = gorm.Open(bot.dbDriver, dbURL.DSN)
	if err != nil {
		log.Fatal(err)
	}

	bot.db.AutoMigrate(&Message{})

	bot.tg, err = tb.NewBot(tb.Settings{
		Token:  token,
		Poller: &tb.LongPoller{Timeout: 30 * time.Second},
	})
	if err != nil {
		closeErr := bot.db.Close()
		if closeErr != nil {
			log.WithError(closeErr).Warning("Failed to close database")
		}
		return nil, err
	}

	bot.tg.Handle(tb.OnText, bot.registrar("text"))
	bot.tg.Handle(tb.OnSticker, bot.registrar("sticker"))
	bot.tg.Handle(tb.OnPhoto, bot.registrar("photo"))
	bot.tg.Handle(tb.OnVideo, bot.registrar("video"))
	bot.tg.Handle(tb.OnAudio, bot.registrar("audio"))
	bot.tg.Handle(tb.OnVoice, bot.registrar("voice"))
	bot.tg.Handle(tb.OnDocument, bot.registrar("document"))
	bot.tg.Handle(tb.OnLocation, bot.registrar("location"))
	bot.tg.Handle(tb.OnContact, bot.registrar("contact"))
	return bot, nil
}

func (bot *Bot) registrar(messageType string) func(m *tb.Message) {
	return func(m *tb.Message) {
		bot.registerMessage(messageType, m)
	}
}

// Start runs the bot.
func (bot *Bot) Start() {
	if release == "" {
		release = "unknown"
	}
	log.WithField("Release", release).Info("Started")
	go bot.startSweeper()
	bot.tg.Start()
}

func (bot *Bot) startSweeper() {
	bot.sweepMessages()
	ticker := time.NewTicker(bot.SweepInterval)
	for range ticker.C {
		bot.sweepMessages()
	}
}

func (bot *Bot) sweepMessages() {
	var messages []Message

	cutoff := time.Now().Add(-bot.MessageTTL)
	log.WithField("Cutoff", cutoff).Info("Sweeping messages")

	tx := bot.db.Begin()
	query := tx
	if bot.dbDriver != "sqlite3" {
		query = query.Set("gorm:query_option", "FOR UPDATE")
	}
	query.Where("time < ? AND NOT deleted", cutoff).Find(&messages)
	for _, msg := range messages {
		mLog := log.WithFields(log.Fields{
			"Type":   msg.Type,
			"ID":     msg.ID,
			"ChatID": msg.ChatID,
		})
		mLog.Info("Deleting message")
		err := bot.tg.Delete(&msg)
		if err != nil {
			mLog.WithError(err).Printf("Failed to delete message")
			okSuffixes := []string{
				"message can't be deleted",
				"message to delete not found",
			}
			if !hasAnySuffix(err.Error(), okSuffixes) {
				continue
			}
		}
		msg.Deleted = true
		tx.Save(&msg)
	}
	tx.Commit()
}

func (bot *Bot) registerMessage(messageType string, m *tb.Message) {
	if m.Chat == nil {
		log.WithField("Type", messageType).
			Warning("Ignoring Message with no associated Chat")
		return
	}
	mLog := log.WithFields(log.Fields{
		"Type":   messageType,
		"ID":     m.ID,
		"ChatID": m.Chat.ID,
	})
	mLog.Info("Registering message")
	db := bot.db.Create(&Message{
		MessageID: m.ID,
		ChatID:    m.Chat.ID,
		Time:      m.Time(),
		Type:      messageType,
	})
	if db.Error != nil {
		mLog.WithError(db.Error).Error("Failed to register message")
	}
}

func main() {
	cfgProviders := []config.Provider{
		config.NewEnvironment(map[string]string{
			"JANITOR_TOKEN": "janitor.token",
			"JANITOR_DB":    "janitor.database",
			"JANITOR_TTL":   "janitor.ttl",
		}),
	}
	iniFilePath := os.Getenv("JANITOR_INI_PATH")
	if iniFilePath == "" {
		iniFilePath = "janitor.ini"
	}
	if _, err := os.Stat(iniFilePath); err == nil {
		cfgProviders = append(
			[]config.Provider{config.NewINIFile(iniFilePath)},
			cfgProviders...,
		)
	}
	cfg := config.NewConfig(cfgProviders)
	cfg.Validate = func(settings map[string]string) error {
		if val, ok := settings["janitor.token"]; !ok || val == "" {
			return errors.New("required setting 'janitor.token' is not set")
		}
		return nil
	}

	bot, err := NewBot(cfg)
	if err != nil {
		log.WithError(err).Fatal("Initialization failed")
	}
	bot.Start()
}
