package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/server"
	"github.com/sartoopjj/thefeed/internal/web"
)

func findFreePort(t *testing.T, network string) int {
	t.Helper()
	switch network {
	case "udp":
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("find free udp port: %v", err)
		}
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).Port
	default:
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("find free tcp port: %v", err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
}

func startDNSServer(t *testing.T, domain, passphrase string, channels []string, messages map[int][]protocol.Message) (string, context.CancelFunc) {
	return startDNSServerWithManage(t, domain, passphrase, false, channels, messages)
}

func startDNSServerWithManage(t *testing.T, domain, passphrase string, allowManage bool, channels []string, messages map[int][]protocol.Message) (string, context.CancelFunc) {
	t.Helper()

	qk, rk, err := protocol.DeriveKeys(passphrase)
	if err != nil {
		t.Fatalf("derive keys: %v", err)
	}

	feed := server.NewFeed(channels)
	for ch, msgs := range messages {
		feed.UpdateChannel(ch, msgs)
	}

	port := findFreePort(t, "udp")
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	channelsFile := ""
	if allowManage {
		f, err := os.CreateTemp(t.TempDir(), "channels-*.txt")
		if err != nil {
			t.Fatalf("create temp channels file: %v", err)
		}
		for _, ch := range channels {
			fmt.Fprintf(f, "@%s\n", ch)
		}
		f.Close()
		channelsFile = f.Name()
	}

	dnsServer := server.NewDNSServer(addr, domain, feed, qk, rk, protocol.DefaultMaxPadding, nil, allowManage, channelsFile)

	ctx, cancel := context.WithCancel(context.Background())

	ready := make(chan struct{})
	go func() {
		close(ready)
		if err := dnsServer.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("dns server error: %v", err)
		}
	}()
	<-ready
	time.Sleep(100 * time.Millisecond)

	return addr, cancel
}

// --- Server E2E Tests ---

func TestE2E_FetchMetadataThroughDNS(t *testing.T) {
	domain := "feed.example.com"
	passphrase := "test-secret-key-123"
	channels := []string{"news", "tech"}

	msgs := map[int][]protocol.Message{
		1: {
			{ID: 100, Timestamp: 1700000000, Text: "Hello from news"},
			{ID: 101, Timestamp: 1700000001, Text: "Second news"},
		},
		2: {
			{ID: 200, Timestamp: 1700000010, Text: "Tech update"},
		},
	}

	resolver, cancel := startDNSServer(t, domain, passphrase, channels, msgs)
	defer cancel()

	fetcher, err := client.NewFetcher(domain, passphrase, []string{resolver})
	if err != nil {
		t.Fatalf("create fetcher: %v", err)
	}

	meta, err := fetcher.FetchMetadata(context.Background())
	if err != nil {
		t.Fatalf("fetch metadata: %v", err)
	}

	if len(meta.Channels) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(meta.Channels))
	}
	if meta.Channels[0].Name != "news" {
		t.Errorf("channel 0 name = %q, want %q", meta.Channels[0].Name, "news")
	}
	if meta.Channels[1].Name != "tech" {
		t.Errorf("channel 1 name = %q, want %q", meta.Channels[1].Name, "tech")
	}
	if meta.Channels[0].LastMsgID != 100 {
		t.Errorf("channel 0 lastMsgID = %d, want 100", meta.Channels[0].LastMsgID)
	}
}

func TestE2E_FetchChannelMessages(t *testing.T) {
	domain := "feed.example.com"
	passphrase := "e2e-pass-456"
	channels := []string{"updates"}

	msgs := map[int][]protocol.Message{
		1: {
			{ID: 1, Timestamp: 1700000000, Text: "First message"},
			{ID: 2, Timestamp: 1700000001, Text: "Second message"},
			{ID: 3, Timestamp: 1700000002, Text: "Third message"},
		},
	}

	resolver, cancel := startDNSServer(t, domain, passphrase, channels, msgs)
	defer cancel()

	fetcher, err := client.NewFetcher(domain, passphrase, []string{resolver})
	if err != nil {
		t.Fatalf("create fetcher: %v", err)
	}

	meta, err := fetcher.FetchMetadata(context.Background())
	if err != nil {
		t.Fatalf("fetch metadata: %v", err)
	}

	blockCount := int(meta.Channels[0].Blocks)
	if blockCount <= 0 {
		t.Fatal("expected blocks > 0")
	}

	fetchedMsgs, err := fetcher.FetchChannel(context.Background(), 1, blockCount)
	if err != nil {
		t.Fatalf("fetch channel: %v", err)
	}

	if len(fetchedMsgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(fetchedMsgs))
	}

	for i, want := range msgs[1] {
		got := fetchedMsgs[i]
		if got.ID != want.ID || got.Text != want.Text {
			t.Errorf("message %d: got {ID:%d Text:%q}, want {ID:%d Text:%q}",
				i, got.ID, got.Text, want.ID, want.Text)
		}
	}
}

func TestE2E_FetchWithDoubleLabel(t *testing.T) {
	domain := "feed.example.com"
	passphrase := "double-label-test"
	channels := []string{"channel1"}

	msgs := map[int][]protocol.Message{
		1: {{ID: 10, Timestamp: 1700000000, Text: "Double label message"}},
	}

	resolver, cancel := startDNSServer(t, domain, passphrase, channels, msgs)
	defer cancel()

	fetcher, err := client.NewFetcher(domain, passphrase, []string{resolver})
	if err != nil {
		t.Fatalf("create fetcher: %v", err)
	}
	fetcher.SetQueryMode(protocol.QueryMultiLabel)

	meta, err := fetcher.FetchMetadata(context.Background())
	if err != nil {
		t.Fatalf("fetch metadata: %v", err)
	}

	fetchedMsgs, err := fetcher.FetchChannel(context.Background(), 1, int(meta.Channels[0].Blocks))
	if err != nil {
		t.Fatalf("fetch channel: %v", err)
	}

	if len(fetchedMsgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(fetchedMsgs))
	}
	if fetchedMsgs[0].Text != "Double label message" {
		t.Errorf("message text = %q, want %q", fetchedMsgs[0].Text, "Double label message")
	}
}

func TestE2E_WrongPassphrase(t *testing.T) {
	domain := "feed.example.com"
	channels := []string{"ch1"}

	msgs := map[int][]protocol.Message{
		1: {{ID: 1, Timestamp: 1700000000, Text: "secret"}},
	}

	resolver, cancel := startDNSServer(t, domain, "server-key", channels, msgs)
	defer cancel()

	fetcher, err := client.NewFetcher(domain, "wrong-key", []string{resolver})
	if err != nil {
		t.Fatalf("create fetcher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = fetcher.FetchMetadata(ctx)
	if err == nil {
		t.Fatal("expected error with wrong passphrase, got nil")
	}
}

func TestE2E_LargeMessages(t *testing.T) {
	domain := "feed.example.com"
	passphrase := "large-msg-test"
	channels := []string{"big"}

	longText := strings.Repeat("A", 500)
	msgs := map[int][]protocol.Message{
		1: {
			{ID: 1, Timestamp: 1700000000, Text: longText},
			{ID: 2, Timestamp: 1700000001, Text: "Short"},
		},
	}

	resolver, cancel := startDNSServer(t, domain, passphrase, channels, msgs)
	defer cancel()

	fetcher, err := client.NewFetcher(domain, passphrase, []string{resolver})
	if err != nil {
		t.Fatalf("create fetcher: %v", err)
	}

	meta, err := fetcher.FetchMetadata(context.Background())
	if err != nil {
		t.Fatalf("fetch metadata: %v", err)
	}

	fetchedMsgs, err := fetcher.FetchChannel(context.Background(), 1, int(meta.Channels[0].Blocks))
	if err != nil {
		t.Fatalf("fetch channel: %v", err)
	}

	if len(fetchedMsgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(fetchedMsgs))
	}
	if fetchedMsgs[0].Text != longText {
		t.Errorf("long message length = %d, want %d", len(fetchedMsgs[0].Text), len(longText))
	}
}

// --- Web UI E2E Tests ---

func TestE2E_WebAPI_ConfigAndStatus(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Status should show not configured
	resp, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()

	var status map[string]any
	json.NewDecoder(resp.Body).Decode(&status)
	if status["configured"] != false {
		t.Errorf("expected configured=false, got %v", status["configured"])
	}

	// GET config when not configured
	resp2, err := http.Get(base + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config: %v", err)
	}
	defer resp2.Body.Close()
	var cfgResp map[string]any
	json.NewDecoder(resp2.Body).Decode(&cfgResp)
	if cfgResp["configured"] != false {
		t.Errorf("expected configured=false on GET config, got %v", cfgResp["configured"])
	}

	// POST config
	cfg := `{"domain":"test.example.com","key":"testpass","resolvers":["127.0.0.1:9999"],"queryMode":"single","rateLimit":10}`
	resp3, err := http.Post(base+"/api/config", "application/json", strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("POST /api/config: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		body, _ := io.ReadAll(resp3.Body)
		t.Fatalf("POST /api/config status=%d body=%s", resp3.StatusCode, body)
	}

	// Status should now show configured
	resp4, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status after config: %v", err)
	}
	defer resp4.Body.Close()
	var status2 map[string]any
	json.NewDecoder(resp4.Body).Decode(&status2)
	if status2["configured"] != true {
		t.Errorf("expected configured=true, got %v", status2["configured"])
	}
	if status2["domain"] != "test.example.com" {
		t.Errorf("domain = %v, want test.example.com", status2["domain"])
	}
}

func TestE2E_WebAPI_InvalidConfig(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Missing required fields
	resp, err := http.Post(base+"/api/config", "application/json", strings.NewReader(`{"domain":"x"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	// Invalid JSON
	resp2, err := http.Post(base+"/api/config", "application/json", strings.NewReader(`not json`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Errorf("expected 400 for invalid json, got %d", resp2.StatusCode)
	}
}

func TestE2E_WebAPI_Channels(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Get(base + "/api/channels")
	if err != nil {
		t.Fatalf("GET /api/channels: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "null\n" && string(body) != "[]\n" {
		t.Logf("channels response: %q (acceptable)", string(body))
	}
}

func TestE2E_WebAPI_Messages(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Get(base + "/api/messages/1")
	if err != nil {
		t.Fatalf("GET /api/messages/1: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	// Invalid channel number
	resp2, err := http.Get(base + "/api/messages/abc")
	if err != nil {
		t.Fatalf("GET /api/messages/abc: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Errorf("expected 400 for invalid channel, got %d", resp2.StatusCode)
	}
}

func TestE2E_WebAPI_IndexPage(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestE2E_WebAPI_NotFound(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Get(base + "/nonexistent")
	if err != nil {
		t.Fatalf("GET /nonexistent: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestE2E_WebAPI_MethodNotAllowed(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	req, _ := http.NewRequest(http.MethodPut, base+"/api/config", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /api/config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}

	resp2, err := http.Get(base + "/api/refresh")
	if err != nil {
		t.Fatalf("GET /api/refresh: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 405 {
		t.Errorf("expected 405 for GET /api/refresh, got %d", resp2.StatusCode)
	}
}

func TestE2E_WebAPI_ConfigPersistence(t *testing.T) {
	dataDir := t.TempDir()

	port1 := findFreePort(t, "tcp")
	srv1, err := web.New(dataDir, port1, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv1.Run()
	time.Sleep(200 * time.Millisecond)

	base1 := fmt.Sprintf("http://127.0.0.1:%d", port1)
	cfg := `{"domain":"persist.example.com","key":"persistkey","resolvers":["1.1.1.1"]}`
	resp, err := http.Post(base1+"/api/config", "application/json", strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("POST config: %v", err)
	}
	resp.Body.Close()

	configPath := dataDir + "/config.json"
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatal("config.json was not persisted to disk")
	}

	port2 := findFreePort(t, "tcp")
	srv2, err := web.New(dataDir, port2, "")
	if err != nil {
		t.Fatalf("create second web server: %v", err)
	}
	go srv2.Run()
	time.Sleep(200 * time.Millisecond)

	base2 := fmt.Sprintf("http://127.0.0.1:%d", port2)
	resp2, err := http.Get(base2 + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status on second instance: %v", err)
	}
	defer resp2.Body.Close()

	var status map[string]any
	json.NewDecoder(resp2.Body).Decode(&status)
	if status["configured"] != true {
		t.Error("second instance should have loaded config, got configured=false")
	}
	if status["domain"] != "persist.example.com" {
		t.Errorf("domain = %v, want persist.example.com", status["domain"])
	}
}

// TestE2E_FullRoundTrip tests DNS server -> client fetcher -> web API end to end.
func TestE2E_FullRoundTrip(t *testing.T) {
	domain := "roundtrip.example.com"
	passphrase := "full-roundtrip-key"
	channels := []string{"general", "alerts"}

	msgs := map[int][]protocol.Message{
		1: {
			{ID: 1, Timestamp: 1700000000, Text: "General message 1"},
			{ID: 2, Timestamp: 1700000001, Text: "General message 2"},
		},
		2: {
			{ID: 10, Timestamp: 1700000010, Text: "Alert!"},
		},
	}

	resolver, cancel := startDNSServer(t, domain, passphrase, channels, msgs)
	defer cancel()

	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	cfgJSON := fmt.Sprintf(`{"domain":"%s","key":"%s","resolvers":["%s"],"queryMode":"single","rateLimit":0}`,
		domain, passphrase, resolver)
	resp, err := http.Post(base+"/api/config", "application/json", strings.NewReader(cfgJSON))
	if err != nil {
		t.Fatalf("POST /api/config: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("config POST status=%d", resp.StatusCode)
	}

	// Refresh channels via selected-channel API semantics.
	respRefresh1, err := http.Post(base+"/api/refresh?channel=1", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/refresh?channel=1: %v", err)
	}
	respRefresh1.Body.Close()
	// Give channel 1 refresh goroutine time to complete before refreshing channel 2,
	// because starting a new refresh cancels the previous in-flight refresh.
	time.Sleep(700 * time.Millisecond)

	// Channels should be populated
	resp2, err := http.Get(base + "/api/channels")
	if err != nil {
		t.Fatalf("GET /api/channels: %v", err)
	}
	defer resp2.Body.Close()

	var chList []protocol.ChannelInfo
	json.NewDecoder(resp2.Body).Decode(&chList)
	if len(chList) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(chList))
	}
	if chList[0].Name != "general" || chList[1].Name != "alerts" {
		t.Errorf("channels = %v, want [general, alerts]", chList)
	}

	// Messages for channel 1
	resp3, err := http.Get(base + "/api/messages/1")
	if err != nil {
		t.Fatalf("GET /api/messages/1: %v", err)
	}
	defer resp3.Body.Close()

	var msgList []protocol.Message
	json.NewDecoder(resp3.Body).Decode(&msgList)
	if len(msgList) != 2 {
		t.Fatalf("expected 2 messages for channel 1, got %d", len(msgList))
	}
	if msgList[0].Text != "General message 1" {
		t.Errorf("msg[0].Text = %q, want %q", msgList[0].Text, "General message 1")
	}

	respRefresh2, err := http.Post(base+"/api/refresh?channel=2", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/refresh?channel=2: %v", err)
	}
	respRefresh2.Body.Close()
	time.Sleep(700 * time.Millisecond)

	// Messages for channel 2
	resp4, err := http.Get(base + "/api/messages/2")
	if err != nil {
		t.Fatalf("GET /api/messages/2: %v", err)
	}
	defer resp4.Body.Close()

	var msgList2 []protocol.Message
	json.NewDecoder(resp4.Body).Decode(&msgList2)
	if len(msgList2) != 1 {
		t.Fatalf("expected 1 message for channel 2, got %d", len(msgList2))
	}
	if msgList2[0].Text != "Alert!" {
		t.Errorf("msg[0].Text = %q, want %q", msgList2[0].Text, "Alert!")
	}
}

// --- Auth E2E Tests ---

func TestE2E_WebAPI_GlobalAuth(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	password := "webpass123"
	srv, err := web.New(dataDir, port, password)
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// All endpoints should require auth when password is set.
	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/"},
		{"GET", "/api/status"},
		{"GET", "/api/config"},
		{"GET", "/api/channels"},
		{"GET", "/api/messages/1"},
		{"GET", "/api/events"},
	}
	for _, ep := range endpoints {
		req, _ := http.NewRequest(ep.method, base+ep.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", ep.method, ep.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s %s without auth: expected 401, got %d", ep.method, ep.path, resp.StatusCode)
		}
	}

	// With correct password, should succeed.
	for _, ep := range endpoints[:5] { // skip /api/events (SSE stream)
		req, _ := http.NewRequest(ep.method, base+ep.path, nil)
		req.SetBasicAuth("", password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", ep.method, ep.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == 401 {
			t.Errorf("%s %s with correct auth: got 401", ep.method, ep.path)
		}
	}

	// Wrong password should be rejected.
	req, _ := http.NewRequest("GET", base+"/api/status", nil)
	req.SetBasicAuth("", "wrongpass")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/status wrong pw: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("wrong password: expected 401, got %d", resp.StatusCode)
	}
}

func TestE2E_AdminAllowManage(t *testing.T) {
	domain := "manage.example.com"
	passphrase := "manage-test"
	channels := []string{"moderated"}

	msgs := map[int][]protocol.Message{
		1: {{ID: 1, Timestamp: 1700000000, Text: "Existing"}},
	}

	resolver, cancel := startDNSServerWithManage(t, domain, passphrase, true, channels, msgs)
	defer cancel()

	fetcher, err := client.NewFetcher(domain, passphrase, []string{resolver})
	if err != nil {
		t.Fatalf("create fetcher: %v", err)
	}

	ctx, ctxCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ctxCancel()

	// Admin list_channels should succeed when allow-manage is enabled.
	result, err := fetcher.SendAdminCommand(ctx, protocol.AdminCmdListChannels, "")
	if err != nil {
		t.Fatalf("expected admin command to succeed with allow-manage, got: %v", err)
	}
	if !strings.Contains(result, "moderated") {
		t.Errorf("expected channel list to contain 'moderated', got: %q", result)
	}
}

func TestE2E_AdminNoManage(t *testing.T) {
	domain := "nomanage.example.com"
	passphrase := "no-manage-test"
	channels := []string{"public"}

	msgs := map[int][]protocol.Message{
		1: {{ID: 1, Timestamp: 1700000000, Text: "Public msg"}},
	}

	// Server has allow-manage disabled — admin commands should be refused.
	resolver, cancel := startDNSServer(t, domain, passphrase, channels, msgs)
	defer cancel()

	fetcher, err := client.NewFetcher(domain, passphrase, []string{resolver})
	if err != nil {
		t.Fatalf("create fetcher: %v", err)
	}

	ctx, ctxCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ctxCancel()

	_, err = fetcher.SendAdminCommand(ctx, protocol.AdminCmdListChannels, "")
	if err == nil {
		t.Error("expected error when server has allow-manage disabled, got nil")
	}
}

// --- Profiles API Tests ---

func startWebServer(t *testing.T) (string, *web.Server) {
	t.Helper()
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)
	return fmt.Sprintf("http://127.0.0.1:%d", port), srv
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func getJSON(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

func TestE2E_Profiles_GetEmpty(t *testing.T) {
	base, _ := startWebServer(t)

	resp := getJSON(t, base+"/api/profiles")
	m := decodeJSON(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if m["profiles"] != nil {
		t.Errorf("expected profiles=null on fresh server, got %v", m["profiles"])
	}
}

func TestE2E_Profiles_CreateAndGet(t *testing.T) {
	base, _ := startWebServer(t)

	body := `{"action":"create","profile":{"id":"","nickname":"Test","config":{"domain":"test.example","key":"mypass","resolvers":["8.8.8.8"],"queryMode":"single","rateLimit":5}}}`
	resp := postJSON(t, base+"/api/profiles", body)
	m := decodeJSON(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("create profile: expected 200, got %d", resp.StatusCode)
	}
	if m["ok"] != true {
		t.Errorf("expected ok=true, got %v", m["ok"])
	}

	// GET should now return the created profile
	resp2 := getJSON(t, base+"/api/profiles")
	m2 := decodeJSON(t, resp2)
	profs, ok := m2["profiles"].([]any)
	if !ok || len(profs) != 1 {
		t.Fatalf("expected 1 profile, got %v", m2["profiles"])
	}
	p := profs[0].(map[string]any)
	if p["nickname"] != "Test" {
		t.Errorf("nickname = %v, want Test", p["nickname"])
	}
	cfg := p["config"].(map[string]any)
	if cfg["domain"] != "test.example" {
		t.Errorf("domain = %v, want test.example", cfg["domain"])
	}
}

func TestE2E_Profiles_CreateSetsActive(t *testing.T) {
	base, _ := startWebServer(t)

	body := `{"action":"create","profile":{"id":"","nickname":"First","config":{"domain":"first.example","key":"k1","resolvers":["1.1.1.1"],"queryMode":"single","rateLimit":0}}}`
	resp := postJSON(t, base+"/api/profiles", body)
	decodeJSON(t, resp)

	resp2 := getJSON(t, base+"/api/profiles")
	m2 := decodeJSON(t, resp2)
	active, _ := m2["active"].(string)
	profs := m2["profiles"].([]any)
	firstID := profs[0].(map[string]any)["id"].(string)
	if active != firstID {
		t.Errorf("first profile should be active, active=%q id=%q", active, firstID)
	}
}

func TestE2E_Profiles_UpdateNickname(t *testing.T) {
	base, _ := startWebServer(t)

	// Create
	createBody := `{"action":"create","profile":{"id":"","nickname":"OldName","config":{"domain":"upd.example","key":"k1","resolvers":["1.1.1.1"],"queryMode":"single","rateLimit":0}}}`
	postJSON(t, base+"/api/profiles", createBody).Body.Close()

	// Get the ID
	m := decodeJSON(t, getJSON(t, base+"/api/profiles"))
	id := m["profiles"].([]any)[0].(map[string]any)["id"].(string)

	updateBody := fmt.Sprintf(`{"action":"update","profile":{"id":%q,"nickname":"NewName","config":{"domain":"upd.example","key":"k1","resolvers":["1.1.1.1"],"queryMode":"single","rateLimit":0}}}`, id)
	resp := postJSON(t, base+"/api/profiles", updateBody)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("update: expected 200, got %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()

	m2 := decodeJSON(t, getJSON(t, base+"/api/profiles"))
	nick := m2["profiles"].([]any)[0].(map[string]any)["nickname"].(string)
	if nick != "NewName" {
		t.Errorf("nickname after update = %q, want NewName", nick)
	}
}

func TestE2E_Profiles_Delete(t *testing.T) {
	base, _ := startWebServer(t)

	postJSON(t, base+"/api/profiles", `{"action":"create","profile":{"id":"","nickname":"ToDelete","config":{"domain":"del.example","key":"k","resolvers":["1.1.1.1"],"queryMode":"single","rateLimit":0}}}`).Body.Close()
	m := decodeJSON(t, getJSON(t, base+"/api/profiles"))
	id := m["profiles"].([]any)[0].(map[string]any)["id"].(string)

	delBody := fmt.Sprintf(`{"action":"delete","profile":{"id":%q}}`, id)
	resp := postJSON(t, base+"/api/profiles", delBody)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	m2 := decodeJSON(t, getJSON(t, base+"/api/profiles"))
	if profs := m2["profiles"]; profs != nil {
		if list, ok := profs.([]any); ok && len(list) != 0 {
			t.Errorf("expected 0 profiles after delete, got %d", len(list))
		}
	}
}

func TestE2E_Profiles_Switch(t *testing.T) {
	base, _ := startWebServer(t)

	postJSON(t, base+"/api/profiles", `{"action":"create","profile":{"id":"","nickname":"A","config":{"domain":"a.example","key":"k","resolvers":["1.1.1.1"],"queryMode":"single","rateLimit":0}}}`).Body.Close()
	postJSON(t, base+"/api/profiles", `{"action":"create","profile":{"id":"","nickname":"B","config":{"domain":"b.example","key":"k","resolvers":["1.1.1.1"],"queryMode":"single","rateLimit":0}}}`).Body.Close()

	m := decodeJSON(t, getJSON(t, base+"/api/profiles"))
	profs := m["profiles"].([]any)
	if len(profs) < 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profs))
	}
	idB := profs[1].(map[string]any)["id"].(string)

	switchBody := fmt.Sprintf(`{"id":%q}`, idB)
	resp := postJSON(t, base+"/api/profiles/switch", switchBody)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("switch: expected 200, got %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()

	m2 := decodeJSON(t, getJSON(t, base+"/api/profiles"))
	if m2["active"] != idB {
		t.Errorf("active after switch = %v, want %q", m2["active"], idB)
	}
}

func TestE2E_Profiles_InvalidAction(t *testing.T) {
	base, _ := startWebServer(t)

	resp := postJSON(t, base+"/api/profiles", `{"action":"bogus","profile":{}}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("bogus action: expected 400, got %d", resp.StatusCode)
	}
}

func TestE2E_Profiles_SwitchNotFound(t *testing.T) {
	base, _ := startWebServer(t)

	resp := postJSON(t, base+"/api/profiles/switch", `{"id":"nonexistent-id"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 && resp.StatusCode != 404 {
		t.Errorf("switch nonexistent: expected 400/404, got %d", resp.StatusCode)
	}
}

// --- Settings API Tests ---

func TestE2E_Settings_GetDefault(t *testing.T) {
	base, _ := startWebServer(t)

	resp := getJSON(t, base+"/api/settings")
	m := decodeJSON(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/settings: expected 200, got %d", resp.StatusCode)
	}
	// fontSize defaults to 0 (use browser default), debug defaults to false
	if _, ok := m["fontSize"]; !ok {
		t.Error("expected 'fontSize' key in settings response")
	}
	if _, ok := m["debug"]; !ok {
		t.Error("expected 'debug' key in settings response")
	}
}

func TestE2E_Settings_SaveAndRead(t *testing.T) {
	base, _ := startWebServer(t)

	resp := postJSON(t, base+"/api/settings", `{"fontSize":16,"debug":true}`)
	m := decodeJSON(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/settings: expected 200, got %d", resp.StatusCode)
	}
	if m["ok"] != true {
		t.Errorf("expected ok=true, got %v", m["ok"])
	}

	m2 := decodeJSON(t, getJSON(t, base+"/api/settings"))
	if m2["fontSize"] != float64(16) {
		t.Errorf("fontSize = %v, want 16", m2["fontSize"])
	}
	if m2["debug"] != true {
		t.Errorf("debug = %v, want true", m2["debug"])
	}
}

func TestE2E_Settings_FontSizeClamped(t *testing.T) {
	base, _ := startWebServer(t)

	// Below minimum
	postJSON(t, base+"/api/settings", `{"fontSize":1}`).Body.Close()
	m := decodeJSON(t, getJSON(t, base+"/api/settings"))
	if want := float64(0); m["fontSize"] != want {
		t.Errorf("fontSize below min: got %v, want %v", m["fontSize"], want)
	}

	// Above maximum (24)
	postJSON(t, base+"/api/settings", `{"fontSize":99}`).Body.Close()
	m2 := decodeJSON(t, getJSON(t, base+"/api/settings"))
	if m2["fontSize"] != float64(24) {
		t.Errorf("fontSize above max: got %v, want 24", m2["fontSize"])
	}
}

func TestE2E_Settings_Persistence(t *testing.T) {
	dataDir := t.TempDir()

	port1 := findFreePort(t, "tcp")
	srv1, err := web.New(dataDir, port1, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv1.Run()
	time.Sleep(200 * time.Millisecond)
	base1 := fmt.Sprintf("http://127.0.0.1:%d", port1)
	postJSON(t, base1+"/api/settings", `{"fontSize":18,"debug":false}`).Body.Close()

	port2 := findFreePort(t, "tcp")
	srv2, err := web.New(dataDir, port2, "")
	if err != nil {
		t.Fatalf("create second web server: %v", err)
	}
	go srv2.Run()
	time.Sleep(200 * time.Millisecond)
	base2 := fmt.Sprintf("http://127.0.0.1:%d", port2)

	m := decodeJSON(t, getJSON(t, base2+"/api/settings"))
	if m["fontSize"] != float64(18) {
		t.Errorf("persisted fontSize = %v, want 18", m["fontSize"])
	}
}

func TestE2E_Settings_MethodNotAllowed(t *testing.T) {
	base, _ := startWebServer(t)

	req, _ := http.NewRequest(http.MethodDelete, base+"/api/settings", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /api/settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}
