package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// TelegramConfig holds Telegram API credentials.
type TelegramConfig struct {
	APIID       int
	APIHash     string
	Phone       string
	Password    string // 2FA password, empty if not used
	SessionPath string
	LoginOnly   bool // if true, authenticate and exit
	CodePrompt  func(ctx context.Context) (string, error)
}

// fileSessionStorage persists gotd session to a JSON file.
type fileSessionStorage struct {
	path string
}

func (f *fileSessionStorage) LoadSession(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, session.ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

func (f *fileSessionStorage) StoreSession(_ context.Context, data []byte) error {
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(f.path, data, 0600)
}

// TelegramReader fetches messages from Telegram channels.
type TelegramReader struct {
	cfg      TelegramConfig
	channels []string // channel usernames without @
	feed     *Feed

	mu       sync.RWMutex
	cache    map[string]cachedMessages
	cacheTTL time.Duration
}

type cachedMessages struct {
	msgs    []protocol.Message
	fetched time.Time
}

// NewTelegramReader creates a reader for the given channel usernames.
func NewTelegramReader(cfg TelegramConfig, channelUsernames []string, feed *Feed) *TelegramReader {
	cleaned := make([]string, len(channelUsernames))
	for i, u := range channelUsernames {
		cleaned[i] = strings.TrimPrefix(strings.TrimSpace(u), "@")
	}
	return &TelegramReader{
		cfg:      cfg,
		channels: cleaned,
		feed:     feed,
		cache:    make(map[string]cachedMessages),
		cacheTTL: 5 * time.Minute,
	}
}

// Run starts the Telegram client, authenticates, and periodically fetches messages.
func (tr *TelegramReader) Run(ctx context.Context) error {
	opts := telegram.Options{}

	// Persist session to file if path is configured
	if tr.cfg.SessionPath != "" {
		opts.SessionStorage = &fileSessionStorage{path: tr.cfg.SessionPath}
	}

	client := telegram.NewClient(tr.cfg.APIID, tr.cfg.APIHash, opts)

	return client.Run(ctx, func(ctx context.Context) error {
		// Authenticate
		if err := tr.authenticate(ctx, client); err != nil {
			return fmt.Errorf("telegram auth: %w", err)
		}

		log.Println("[telegram] authenticated successfully")

		// Login-only mode: just authenticate and return
		if tr.cfg.LoginOnly {
			log.Println("[telegram] login-only mode, session saved, exiting")
			return nil
		}

		api := client.API()

		// Initial fetch
		tr.fetchAll(ctx, api)

		// Periodic fetch loop
		ticker := time.NewTicker(3 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				tr.fetchAll(ctx, api)
			}
		}
	})
}

func (tr *TelegramReader) authenticate(ctx context.Context, client *telegram.Client) error {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return err
	}
	if status.Authorized {
		return nil
	}

	codeAuth := auth.CodeAuthenticatorFunc(func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
		if tr.cfg.CodePrompt != nil {
			return tr.cfg.CodePrompt(ctx)
		}
		return "", fmt.Errorf("no code prompt configured")
	})

	var authConv auth.UserAuthenticator
	if tr.cfg.Password != "" {
		authConv = auth.Constant(tr.cfg.Phone, tr.cfg.Password, codeAuth)
	} else {
		authConv = auth.Constant(tr.cfg.Phone, "", codeAuth)
	}

	flow := auth.NewFlow(authConv, auth.SendCodeOptions{})
	return client.Auth().IfNecessary(ctx, flow)
}

func (tr *TelegramReader) fetchAll(ctx context.Context, api *tg.Client) {
	for i, username := range tr.channels {
		chNum := i + 1

		// Check cache
		tr.mu.RLock()
		cached, ok := tr.cache[username]
		tr.mu.RUnlock()
		if ok && time.Since(cached.fetched) < tr.cacheTTL {
			continue
		}

		msgs, err := tr.fetchChannel(ctx, api, username)
		if err != nil {
			log.Printf("[telegram] fetch %s: %v", username, err)
			continue
		}

		// Update cache
		tr.mu.Lock()
		tr.cache[username] = cachedMessages{msgs: msgs, fetched: time.Now()}
		tr.mu.Unlock()

		// Update feed
		tr.feed.UpdateChannel(chNum, msgs)
		log.Printf("[telegram] updated %s: %d messages", username, len(msgs))
	}
}

func (tr *TelegramReader) fetchChannel(ctx context.Context, api *tg.Client, username string) ([]protocol.Message, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: username,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", username, err)
	}

	var channel *tg.Channel
	for _, chat := range resolved.Chats {
		if ch, ok := chat.(*tg.Channel); ok {
			channel = ch
			break
		}
	}
	if channel == nil {
		return nil, fmt.Errorf("channel %s not found in resolved chats", username)
	}

	peer := &tg.InputPeerChannel{
		ChannelID:  channel.ID,
		AccessHash: channel.AccessHash,
	}

	hist, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  peer,
		Limit: 100,
	})
	if err != nil {
		return nil, fmt.Errorf("get history %s: %w", username, err)
	}

	return tr.extractMessages(hist)
}

func (tr *TelegramReader) extractMessages(hist tg.MessagesMessagesClass) ([]protocol.Message, error) {
	var tgMsgs []tg.MessageClass

	switch h := hist.(type) {
	case *tg.MessagesMessages:
		tgMsgs = h.Messages
	case *tg.MessagesMessagesSlice:
		tgMsgs = h.Messages
	case *tg.MessagesChannelMessages:
		tgMsgs = h.Messages
	default:
		return nil, fmt.Errorf("unexpected messages type: %T", hist)
	}

	var msgs []protocol.Message
	for _, raw := range tgMsgs {
		msg, ok := raw.(*tg.Message)
		if !ok {
			continue
		}

		text := tr.extractText(msg)
		if text == "" {
			continue
		}

		msgs = append(msgs, protocol.Message{
			ID:        uint32(msg.ID),
			Timestamp: uint32(msg.Date),
			Text:      text,
		})
	}

	return msgs, nil
}

func (tr *TelegramReader) extractText(msg *tg.Message) string {
	text := msg.Message

	mediaPrefix := ""
	if msg.Media != nil {
		switch msg.Media.(type) {
		case *tg.MessageMediaPhoto:
			mediaPrefix = protocol.MediaImage
		case *tg.MessageMediaDocument:
			mediaPrefix = tr.classifyDocument(msg.Media.(*tg.MessageMediaDocument))
		case *tg.MessageMediaGeo, *tg.MessageMediaGeoLive, *tg.MessageMediaVenue:
			mediaPrefix = protocol.MediaLocation
		case *tg.MessageMediaContact:
			mediaPrefix = protocol.MediaContact
		case *tg.MessageMediaPoll:
			mediaPrefix = protocol.MediaPoll
		}
	}

	if mediaPrefix != "" {
		if text != "" {
			return mediaPrefix + "\n" + text
		}
		return mediaPrefix
	}

	return text
}

func (tr *TelegramReader) classifyDocument(media *tg.MessageMediaDocument) string {
	doc, ok := media.Document.(*tg.Document)
	if !ok {
		return protocol.MediaFile
	}

	for _, attr := range doc.Attributes {
		switch attr.(type) {
		case *tg.DocumentAttributeVideo:
			return protocol.MediaVideo
		case *tg.DocumentAttributeAudio:
			return protocol.MediaAudio
		case *tg.DocumentAttributeSticker:
			return protocol.MediaSticker
		case *tg.DocumentAttributeAnimated:
			return protocol.MediaGIF
		}
	}

	return protocol.MediaFile
}
