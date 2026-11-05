// usage 包从上游响应中提取 OpenAI 兼容的 token 用量。
//
// 非流式响应带一个 usage 对象。流式响应只在最后一个 SSE chunk 中携带，
// 且需要调用方请求 include_usage，因此流式数据交给 StreamTracker：
// 它维护有上限的行缓冲，并记住最后一次看到的 usage 对象。
package usage

import (
	"bytes"
	"encoding/json"
	"sync"
)

// Usage 对应 OpenAI 的 usage 对象。
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// Empty 表示上游没有报告任何 token 用量。
func (u Usage) Empty() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0
}

type payload struct {
	Usage *Usage `json:"usage"`
}

// Parse 从 JSON 响应体中提取 usage。
func Parse(body []byte) (Usage, bool) {
	var decoded payload
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Usage == nil {
		return Usage{}, false
	}
	return *decoded.Usage, true
}

// StreamTracker 增量扫描 SSE 流中的 usage 对象。
type StreamTracker struct {
	mu    sync.Mutex
	buf   []byte
	usage Usage
	found bool
}

const maxLineBytes = 1 << 20

// Write 实现 io.Writer，可以放在客户端 ResponseWriter 前面。
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

	// 上游格式异常时不能无限增长内存。
	if len(t.buf) > maxLineBytes {
		t.buf = t.buf[:0]
	}
	return len(p), nil
}

// Usage 返回流中最后一次观察到的 usage 对象。
func (t *StreamTracker) Usage() (Usage, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.usage, t.found
}

// Reset 清空未完成的行缓冲，但保留已解析的 usage。
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
