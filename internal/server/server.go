package server

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Config holds server configuration.
type Config struct {
	ListenAddr    string
	Domain        string
	Passphrase    string
	ChannelsFile  string
	XAccountsFile string
	XRSSInstances string
	MaxPadding    int
	MsgLimit      int  // max messages per channel (0 = default 15)
	NoTelegram    bool // if true, fetch public channels without Telegram login
	AllowManage   bool // if true, remote channel management and sending via DNS is allowed
	Debug         bool // if true, log every decoded DNS query
	Telegram      TelegramConfig
}

// Server orchestrates the DNS server and Telegram reader.
type Server struct {
	cfg              Config
	feed             *Feed
	reader           *TelegramReader // nil when --no-telegram
	telegramChannels []string
	xAccounts        []string
}

// New creates a new Server.
func New(cfg Config) (*Server, error) {
	channels, err := loadUsernames(cfg.ChannelsFile)
	if err != nil {
		return nil, fmt.Errorf("load channels: %w", err)
	}
	xAccounts, err := loadUsernames(cfg.XAccountsFile)
	if err != nil {
		return nil, fmt.Errorf("load X accounts: %w", err)
	}

	if len(channels) == 0 && len(xAccounts) == 0 {
		return nil, fmt.Errorf("no channels configured in %s and no X accounts configured in %s", cfg.ChannelsFile, cfg.XAccountsFile)
	}

	log.Printf("[server] loaded %d Telegram channels and %d X accounts", len(channels), len(xAccounts))

	feed := NewFeed(append(append([]string{}, channels...), prefixXAccounts(xAccounts)...))
	return &Server{cfg: cfg, feed: feed, telegramChannels: channels, xAccounts: xAccounts}, nil
}

// Run starts both the DNS server and the Telegram reader.
func (s *Server) Run(ctx context.Context) error {
	queryKey, responseKey, err := protocol.DeriveKeys(s.cfg.Passphrase)
	if err != nil {
		return fmt.Errorf("derive keys: %w", err)
	}

	go startLatestVersionTracker(ctx, s.feed)
	var channelCtl channelRefresher

	// Handle login-only mode
	if s.cfg.Telegram.LoginOnly {
		reader := NewTelegramReader(s.cfg.Telegram, s.telegramChannels, s.feed, 15, 1)
		return reader.Run(ctx)
	}

	// Start Telegram reader in background, or public web fetcher in no-login mode.
	if !s.cfg.NoTelegram {
		msgLimit := s.cfg.MsgLimit
		if msgLimit <= 0 {
			msgLimit = 15
		}
		if len(s.telegramChannels) > 0 {
			reader := NewTelegramReader(s.cfg.Telegram, s.telegramChannels, s.feed, msgLimit, 1)
			s.reader = reader
			channelCtl = reader
			go func() {
				if err := reader.Run(ctx); err != nil {
					log.Printf("[telegram] error: %v", err)
				}
			}()
		} else {
			s.feed.SetTelegramLoggedIn(true)
		}
	} else {
		msgLimit := s.cfg.MsgLimit
		if msgLimit <= 0 {
			msgLimit = 15
		}
		publicReader := NewPublicReader(s.telegramChannels, s.feed, msgLimit, 1)
		channelCtl = publicReader
		go func() {
			if err := publicReader.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[public] error: %v", err)
			}
		}()
		log.Println("[server] running without Telegram login; fetching public channels via t.me")
	}

	var xReader *XPublicReader
	if len(s.xAccounts) > 0 {
		msgLimit := s.cfg.MsgLimit
		if msgLimit <= 0 {
			msgLimit = 15
		}
		xReader = NewXPublicReader(s.xAccounts, s.feed, msgLimit, len(s.telegramChannels)+1, s.cfg.XRSSInstances)
		go func() {
			if err := xReader.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[x] error: %v", err)
			}
		}()
		log.Printf("[server] enabled X source for %d accounts", len(s.xAccounts))
	}

	// Start DNS server (blocking, respects ctx cancellation)
	maxPad := s.cfg.MaxPadding
	if maxPad == 0 {
		maxPad = protocol.DefaultMaxPadding
	}
	dnsServer := NewDNSServer(s.cfg.ListenAddr, s.cfg.Domain, s.feed, queryKey, responseKey, maxPad, s.reader, s.cfg.AllowManage, s.cfg.ChannelsFile, s.xAccounts, s.cfg.Debug)
	if channelCtl != nil {
		dnsServer.SetChannelRefresher(channelCtl)
	}
	if xReader != nil {
		dnsServer.AddRefresher(xReader)
	}
	return dnsServer.ListenAndServe(ctx)
}

func loadUsernames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Printf("[server] close usernames file: %v", err)
		}
	}()

	var users []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := strings.TrimPrefix(line, "@")
		users = append(users, name)
	}
	return users, scanner.Err()
}

func prefixXAccounts(accounts []string) []string {
	out := make([]string, len(accounts))
	for i, a := range accounts {
		out[i] = "x/" + a
	}
	return out
}
