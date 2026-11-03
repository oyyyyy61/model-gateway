// Package usage extracts OpenAI compatible token usage from upstream
// responses.
//
// Non streaming responses carry a single usage object. Streaming responses only
// carry it in the final SSE chunk and only when the caller asked for
// include_usage, so the stream is fed through StreamTracker which keeps a
// bounded line buffer and remembers the last usage object it saw.
package usage

import (
	"bytes"
	"encoding/json"
	"sync"
)

// Usage mirrors the OpenAI usage object.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// Empty reports whether the upstream did not report any token usage.
func (u Usage) Empty() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0
}

type payload struct {
	Usage *Usage `json:"usage"`
}

// Parse extracts usage from a JSON response body.
func Parse(body []byte) (Usage, bool) {
	var decoded payload
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Usage == nil {
		return Usage{}, false
	}
	return *decoded.Usage, true
}

// StreamTracker incrementally scans an SSE stream for usage objects.
type StreamTracker struct {
	mu    sync.Mutex
	buf   []byte
	usage Usage
	found bool
}

const maxLineBytes = 1 << 20

// Write implements io.Writer so it can be placed in front of the client
// response writer.
func (t *StreamTracker) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.buf = append(t.buf, p...)
	for {
		index := bytes.IndexByte(t.buf, '\n')
		if index < 0 {
			break
		}
		line := t.buf[:index]
		t.buf = t.buf[index+1:]
		t.consume(line)
	}

	// A malformed upstream must not grow memory without bound.
	if len(t.buf) > maxLineBytes {
		t.buf = t.buf[:0]
	}
	return len(p), nil
}

// Usage returns the last usage object observed in the stream.
func (t *StreamTracker) Usage() (Usage, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.usage, t.found
}

// Reset clears buffered partial lines while preserving parsed usage.
func (t *StreamTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = nil
}

func (t *StreamTracker) consume(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
		return
	}
	if parsed, ok := Parse(line); ok {
		t.usage = parsed
		t.found = true
	}
}
