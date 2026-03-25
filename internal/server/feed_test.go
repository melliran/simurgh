package server

import (
	"testing"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

func TestFeedUpdateAndGetBlock(t *testing.T) {
	feed := NewFeed([]string{"TestChannel"})
	msgs := []protocol.Message{
		{ID: 1, Timestamp: 1700000000, Text: "First message"},
		{ID: 2, Timestamp: 1700000060, Text: "Second message"},
	}
	feed.UpdateChannel(1, msgs)
	data, err := feed.GetBlock(1, 0)
	if err != nil {
		t.Fatalf("GetBlock(1, 0): %v", err)
	}
	if len(data) == 0 {
		t.Error("block data should not be empty")
	}
	parsed, err := protocol.ParseMessages(data)
	if err != nil {
		t.Fatalf("ParseMessages: %v", err)
	}
	if len(parsed) != 2 {
		t.Errorf("got %d messages, want 2", len(parsed))
	}
}

func TestFeedMetadataBlock(t *testing.T) {
	feed := NewFeed([]string{"Channel1", "Channel2"})
	msgs := []protocol.Message{{ID: 10, Timestamp: 1700000000, Text: "Hello"}}
	feed.UpdateChannel(1, msgs)
	data, err := feed.GetBlock(protocol.MetadataChannel, 0)
	if err != nil {
		t.Fatalf("GetBlock(0, 0): %v", err)
	}
	meta, err := protocol.ParseMetadata(data)
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
	}
	if len(meta.Channels) != 2 {
		t.Fatalf("channels: got %d, want 2", len(meta.Channels))
	}
	if meta.Channels[0].Name != "Channel1" {
		t.Errorf("name: got %q, want Channel1", meta.Channels[0].Name)
	}
	if meta.Channels[0].Blocks != 1 {
		t.Errorf("blocks: got %d, want 1", meta.Channels[0].Blocks)
	}
}

func TestFeedGetBlockOutOfRange(t *testing.T) {
	feed := NewFeed([]string{"Test"})
	feed.UpdateChannel(1, []protocol.Message{{ID: 1, Timestamp: 1, Text: "x"}})
	_, err := feed.GetBlock(1, 999)
	if err == nil {
		t.Error("expected error for out-of-range block")
	}
}

func TestFeedGetBlockUnknownChannel(t *testing.T) {
	feed := NewFeed([]string{"Test"})
	_, err := feed.GetBlock(99, 0)
	if err == nil {
		t.Error("expected error for unknown channel")
	}
}

func TestFeedLargeMessages(t *testing.T) {
	feed := NewFeed([]string{"Test"})
	// Use text large enough to span 2 blocks at DefaultBlockPayload (currently 700 bytes).
	// Message serialization overhead is 10 bytes, so we need >690 bytes of text.
	largeText := make([]byte, 750)
	for i := range largeText {
		largeText[i] = 65
	}
	msgs := []protocol.Message{{ID: 1, Timestamp: 1700000000, Text: string(largeText)}}
	feed.UpdateChannel(1, msgs)
	_, err := feed.GetBlock(1, 0)
	if err != nil {
		t.Fatalf("GetBlock(1, 0): %v", err)
	}
	_, err = feed.GetBlock(1, 1)
	if err != nil {
		t.Fatalf("GetBlock(1, 1): %v", err)
	}
}
