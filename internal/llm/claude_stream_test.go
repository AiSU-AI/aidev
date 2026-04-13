package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestClaudeStreamHappyPath stands up a fake SSE server that emits a
// canonical Messages streaming sequence and verifies the channel
// reassembles the full text from individual chunks.
func TestClaudeStreamHappyPath(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {}\n",
		"event: content_block_start\ndata: {}\n",
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello, \"}}\n",
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"world\"}}\n",
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"!\"}}\n",
		"event: content_block_stop\ndata: {}\n",
		"event: message_stop\ndata: {}\n",
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			// Frames are separated by blank lines.
			w.Write([]byte(f))
			w.Write([]byte("\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer ts.Close()

	os.Setenv("ANTHROPIC_API_KEY", "test-key")
	c := &Claude{
		apiKey:      "test-key",
		model:       "claude-test",
		maxTokens:   1024,
		temperature: 0.1,
		endpoint:    ts.URL,
		http:        &http.Client{Timeout: 5 * time.Second},
	}

	chunks, err := c.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	sawDone := false
	for chunk := range chunks {
		if chunk.Err != nil {
			t.Errorf("unexpected error in stream: %v", chunk.Err)
		}
		text.WriteString(chunk.Text)
		if chunk.Done {
			sawDone = true
		}
	}
	if got := text.String(); got != "Hello, world!" {
		t.Errorf("got %q, want 'Hello, world!'", got)
	}
	if !sawDone {
		t.Error("expected a Done chunk")
	}
}

// TestClaudeStreamErrorFrame verifies that an error SSE frame mid-stream
// is surfaced as a Done+Err chunk.
func TestClaudeStreamErrorFrame(t *testing.T) {
	frames := []string{
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n",
		"event: error\ndata: {\"error\":{\"type\":\"overloaded_error\",\"message\":\"try again later\"}}\n",
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			w.Write([]byte(f))
			w.Write([]byte("\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer ts.Close()

	c := &Claude{
		apiKey:   "k",
		endpoint: ts.URL,
		http:     &http.Client{Timeout: 5 * time.Second},
	}
	chunks, err := c.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var sawError bool
	var text strings.Builder
	for chunk := range chunks {
		text.WriteString(chunk.Text)
		if chunk.Err != nil {
			sawError = true
			if !strings.Contains(chunk.Err.Error(), "overloaded_error") {
				t.Errorf("error should mention overloaded_error, got: %v", chunk.Err)
			}
		}
	}
	if text.String() != "partial" {
		t.Errorf("partial text lost: %q", text.String())
	}
	if !sawError {
		t.Error("expected an error chunk")
	}
}

// TestClaudeStreamNoKey verifies fast-fail when the API key is missing.
func TestClaudeStreamNoKey(t *testing.T) {
	c := &Claude{} // empty apiKey
	_, err := c.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Error("expected error when apiKey is empty")
	}
}

// TestClaudeImplementsStreamer is a compile-time check that the Claude
// type satisfies the Streamer interface. If it ever regresses, this
// test fails at build time with a useful error.
func TestClaudeImplementsStreamer(t *testing.T) {
	var _ Streamer = (*Claude)(nil)
}
