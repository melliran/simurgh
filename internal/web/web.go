package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/version"
)

//go:embed static
var staticFS embed.FS

// Config holds the client configuration saved in the data directory.
type Config struct {
	Domain    string   `json:"domain"`
	Key       string   `json:"key"`
	Resolvers []string `json:"resolvers"`
	QueryMode string   `json:"queryMode"`
	RateLimit float64  `json:"rateLimit"`
	// Timeout is the per-query DNS timeout in seconds (0 = default 5 s).
	// Also used as the resolver health-check probe timeout.
	Timeout float64 `json:"timeout,omitempty"`
	// Debug enables verbose query logging (shows generated DNS query names).
	Debug bool `json:"debug,omitempty"`
}

// Server is the web UI server for thefeed client.
type Server struct {
	dataDir string
	port    int

	mu       sync.RWMutex
	config   *Config
	fetcher  *client.Fetcher
	cache    *client.Cache
	channels []protocol.ChannelInfo
	messages map[int][]protocol.Message

	// fetcherCtx/fetcherCancel control the lifetime of the active fetcher's
	// background goroutines (rate limiter, noise, resolver checker).
	// They are cancelled and recreated each time the config changes.
	fetcherCtx    context.Context
	fetcherCancel context.CancelFunc

	// refreshMu / refreshCancel allow a new refresh to cancel an in-progress one.
	refreshMu     sync.Mutex
	refreshCancel context.CancelFunc

	logMu    sync.RWMutex
	logLines []string

	sseMu   sync.Mutex
	clients map[chan string]struct{}

	stopRefresh chan struct{}
}

// New creates a new web server.
func New(dataDir string, port int) (*Server, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	s := &Server{
		dataDir:  dataDir,
		port:     port,
		messages: make(map[int][]protocol.Message),
		clients:  make(map[chan string]struct{}),
	}

	cfg, err := s.loadConfig()
	if err == nil {
		s.config = cfg
		if err := s.initFetcher(); err != nil {
			log.Printf("Warning: could not initialize fetcher: %v", err)
		}
	}

	return s, nil
}

// Run starts the web server.
func (s *Server) Run() error {
	mux := http.NewServeMux()

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/channels", s.handleChannels)
	mux.HandleFunc("/api/messages/", s.handleMessages)
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/", s.handleIndex)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	log.Printf("thefeed client %s", version.Version)
	fmt.Printf("\n  Open in browser: http://%s\n\n", addr)

	if s.fetcher != nil {
		go s.refreshMetadataOnly()
	}

	return http.ListenAndServe(addr, mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	status := map[string]any{
		"configured": s.config != nil,
		"version":    version.Version,
	}
	if s.config != nil {
		status["domain"] = s.config.Domain
		status["channels"] = s.channels
	}
	writeJSON(w, status)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.config == nil {
			writeJSON(w, map[string]any{"configured": false})
			return
		}
		writeJSON(w, s.config)

	case http.MethodPost:
		var cfg Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if cfg.Domain == "" || cfg.Key == "" || len(cfg.Resolvers) == 0 {
			http.Error(w, "domain, key, and resolvers are required", 400)
			return
		}
		if err := s.saveConfig(&cfg); err != nil {
			http.Error(w, fmt.Sprintf("save config: %v", err), 500)
			return
		}
		s.mu.Lock()
		s.config = &cfg
		s.mu.Unlock()

		if err := s.initFetcher(); err != nil {
			http.Error(w, fmt.Sprintf("init fetcher: %v", err), 500)
			return
		}
		go s.refreshMetadataOnly()
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, s.channels)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "missing channel number", 400)
		return
	}
	chNum, err := strconv.Atoi(parts[3])
	if err != nil || chNum < 1 {
		http.Error(w, "invalid channel number", 400)
		return
	}

	s.mu.RLock()
	msgs := s.messages[chNum]
	s.mu.RUnlock()

	writeJSON(w, msgs)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	// Background (quiet) refreshes skip silently if one is already running,
	// so the auto-refresh timer never cancels a slow in-progress fetch.
	if r.URL.Query().Get("quiet") == "1" {
		s.refreshMu.Lock()
		running := s.refreshCancel != nil
		s.refreshMu.Unlock()
		if running {
			writeJSON(w, map[string]any{"ok": true, "skipped": true})
			return
		}
	}
	chParam := r.URL.Query().Get("channel")
	if chParam != "" {
		chNum, err := strconv.Atoi(chParam)
		if err != nil || chNum < 1 {
			http.Error(w, "invalid channel", 400)
			return
		}
		go s.refreshChannel(chNum)
	} else {
		go s.refreshMetadataOnly()
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 100)
	s.sseMu.Lock()
	s.clients[ch] = struct{}{}
	s.sseMu.Unlock()

	defer func() {
		s.sseMu.Lock()
		delete(s.clients, ch)
		s.sseMu.Unlock()
	}()

	s.logMu.RLock()
	for _, line := range s.logLines {
		data, _ := json.Marshal(line)
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
	}
	s.logMu.RUnlock()
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			fmt.Fprint(w, msg)
			flusher.Flush()
		}
	}
}

func (s *Server) broadcast(event string) {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

func (s *Server) addLog(msg string) {
	ts := time.Now().Format("15:04:05")
	line := fmt.Sprintf("%s %s", ts, msg)

	s.logMu.Lock()
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > 200 {
		s.logLines = s.logLines[len(s.logLines)-200:]
	}
	s.logMu.Unlock()

	data, _ := json.Marshal(line)
	s.broadcast(fmt.Sprintf("event: log\ndata: %s\n\n", data))
}

func (s *Server) initFetcher() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cancel goroutines from the previous fetcher configuration.
	if s.fetcherCancel != nil {
		s.fetcherCancel()
	}

	cfg := s.config
	if cfg == nil {
		return fmt.Errorf("no config")
	}

	cacheDir := filepath.Join(s.dataDir, "cache")
	cache, err := client.NewCache(cacheDir)
	if err != nil {
		return fmt.Errorf("create cache: %w", err)
	}

	fetcher, err := client.NewFetcher(cfg.Domain, cfg.Key, cfg.Resolvers)
	if err != nil {
		return fmt.Errorf("create fetcher: %w", err)
	}

	if cfg.QueryMode == "double" {
		fetcher.SetQueryMode(protocol.QueryMultiLabel)
	} else if cfg.QueryMode == "plain" {
		fetcher.SetQueryMode(protocol.QueryPlainLabel)
	}
	fetcher.SetDebug(cfg.Debug)
	if cfg.RateLimit > 0 {
		fetcher.SetRateLimit(cfg.RateLimit)
	}

	timeout := 5 * time.Second
	if cfg.Timeout > 0 {
		timeout = time.Duration(cfg.Timeout * float64(time.Second))
	}
	fetcher.SetTimeout(timeout)

	fetcher.SetLogFunc(func(msg string) {
		s.addLog(msg)
	})

	// Create a shared context for this fetcher's lifetime.
	ctx, cancel := context.WithCancel(context.Background())
	s.fetcherCtx = ctx
	s.fetcherCancel = cancel

	// Start rate limiter and noise goroutines.
	fetcher.Start(ctx)

	// Start periodic resolver health checks.
	checker := client.NewResolverChecker(fetcher, timeout)
	checker.SetLogFunc(func(msg string) {
		s.addLog(msg)
	})
	checker.Start(ctx)

	s.fetcher = fetcher
	s.cache = cache
	return nil
}

func (s *Server) refreshMetadataOnly() {
	// Cancel any in-progress refresh and start a new cancellable one.
	s.refreshMu.Lock()
	if s.refreshCancel != nil {
		s.refreshCancel()
	}

	s.mu.RLock()
	basectx := s.fetcherCtx
	fetcher := s.fetcher
	cache := s.cache
	s.mu.RUnlock()

	if fetcher == nil || basectx == nil {
		s.refreshMu.Unlock()
		return
	}

	// Child context: cancelled either by the next refresh call or by a config change.
	ctx, cancel := context.WithCancel(basectx)
	s.refreshCancel = cancel
	s.refreshMu.Unlock()
	defer func() {
		cancel()
		s.refreshMu.Lock()
		s.refreshCancel = nil
		s.refreshMu.Unlock()
	}()

	s.addLog("Fetching metadata...")
	meta, err := fetcher.FetchMetadata(ctx)
	if err != nil {
		if ctx.Err() != nil {
			s.addLog("Refresh cancelled")
			return
		}
		s.addLog(fmt.Sprintf("Error: %v", err))
		return
	}

	s.mu.Lock()
	s.channels = meta.Channels
	s.mu.Unlock()

	if cache != nil {
		_ = cache.PutMetadata(meta)
	}

	s.broadcast("event: update\ndata: \"channels\"\n\n")
}

func (s *Server) refreshChannel(channelNum int) {
	s.refreshMu.Lock()
	if s.refreshCancel != nil {
		s.refreshCancel()
	}

	s.mu.RLock()
	basectx := s.fetcherCtx
	fetcher := s.fetcher
	cache := s.cache
	channels := s.channels
	s.mu.RUnlock()

	if fetcher == nil || basectx == nil {
		s.refreshMu.Unlock()
		return
	}

	ctx, cancel := context.WithCancel(basectx)
	s.refreshCancel = cancel
	s.refreshMu.Unlock()
	defer func() {
		cancel()
		s.refreshMu.Lock()
		s.refreshCancel = nil
		s.refreshMu.Unlock()
	}()

	meta, err := fetcher.FetchMetadata(ctx)
	if err != nil {
		if ctx.Err() != nil {
			s.addLog("Refresh cancelled")
			return
		}
		s.addLog(fmt.Sprintf("Error: %v", err))
		return
	}

	s.mu.Lock()
	s.channels = meta.Channels
	s.mu.Unlock()

	if cache != nil {
		_ = cache.PutMetadata(meta)
	}
	s.broadcast("event: update\ndata: \"channels\"\n\n")

	channels = meta.Channels
	if channelNum < 1 || channelNum > len(channels) {
		s.addLog(fmt.Sprintf("Warning: channel %d is not available", channelNum))
		return
	}

	ch := channels[channelNum-1]
	blockCount := int(ch.Blocks)
	if blockCount <= 0 {
		s.mu.Lock()
		s.messages[channelNum] = nil
		s.mu.Unlock()
		s.addLog(fmt.Sprintf("Updated %s: 0 messages", ch.Name))
		s.broadcast("event: update\ndata: \"messages\"\n\n")
		return
	}

	msgs, err := fetcher.FetchChannel(ctx, channelNum, blockCount)
	if err != nil {
		if ctx.Err() != nil {
			s.addLog("Refresh cancelled")
			return
		}
		s.addLog(fmt.Sprintf("Channel %s error: %v", ch.Name, err))
		return
	}

	s.mu.Lock()
	s.messages[channelNum] = msgs
	s.mu.Unlock()

	if cache != nil {
		_ = cache.PutMessages(channelNum, msgs)
	}

	s.addLog(fmt.Sprintf("Updated %s: %d messages", ch.Name, len(msgs)))
	s.broadcast("event: update\ndata: \"messages\"\n\n")
}

func (s *Server) loadConfig() (*Config, error) {
	path := filepath.Join(s.dataDir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (s *Server) saveConfig(cfg *Config) error {
	path := filepath.Join(s.dataDir, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
