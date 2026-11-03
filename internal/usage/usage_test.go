package usage

import "testing"

func TestParseJSONUsage(t *testing.T) {
	parsed, ok := Parse([]byte(`{"id":"x","usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
	if !ok || parsed.TotalTokens != 18 || parsed.PromptTokens != 11 || parsed.CompletionTokens != 7 {
		t.Fatalf("unexpected usage: %#v, %v", parsed, ok)
	}
	if _, ok := Parse([]byte(`{"id":"x"}`)); ok {
		t.Fatal("expected missing usage to be reported")
	}
}

func TestStreamTrackerHandlesSplitChunks(t *testing.T) {
	tracker := &StreamTracker{}
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: {\"choices\":[],\"usa",
		"ge\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n",
	}
	for _, chunk := range chunks {
		if _, err := tracker.Write([]byte(chunk)); err != nil {
			t.Fatalf("write failed: %v", err)
		}
	}
	parsed, ok := tracker.Usage()
	if !ok || parsed.TotalTokens != 5 || parsed.PromptTokens != 2 {
		t.Fatalf("unexpected stream usage: %#v, %v", parsed, ok)
	}
}

func TestStreamTrackerKeepsLatestUsage(t *testing.T) {
	tracker := &StreamTracker{}
	_, _ = tracker.Write([]byte("data: {\"usage\":{\"total_tokens\":1}}\n\n"))
	_, _ = tracker.Write([]byte("data: {\"usage\":{\"total_tokens\":9}}\n\n"))
	parsed, ok := tracker.Usage()
	if !ok || parsed.TotalTokens != 9 {
		t.Fatalf("expected the latest usage object, got %#v, %v", parsed, ok)
	}
}
