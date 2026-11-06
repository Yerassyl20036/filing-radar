package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeChat stands in for any chat completions endpoint. reply decides each
// answer from the request; requests are kept for the test to inspect.
type fakeChat struct {
	mu   sync.Mutex
	reqs []map[string]any
	auth []string
}

func (f *fakeChat) server(t *testing.T, reply func(n int, req map[string]any) (int, string)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		n := len(f.reqs)
		f.mu.Unlock()

		status, out := reply(n, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// completion wraps an answer the way chat APIs do.
func completion(model, content, finish string) string {
	b, _ := json.Marshal(map[string]any{
		"object": "chat.completion", "model": model,
		"choices": []map[string]any{{"index": 0, "finish_reason": finish, "message": map[string]any{"role": "assistant", "content": content}}},
	})
	return string(b)
}

const goodAnswer = `{"score": 88, "reason": "Past statements can't be relied on.", "quote": "should no longer be relied upon"}`

var in = Input{Company: "Test Co", FormType: "8-K", Items: []string{"4.02"}, Text: filing}

func TestPresets(t *testing.T) {
	for _, name := range []string{"deepseek", "gemini", "groq"} {
		t.Run(name, func(t *testing.T) {
			p := Presets[name]
			f := &fakeChat{}
			url := f.server(t, func(int, map[string]any) (int, string) { return 200, completion(p.Model, goodAnswer, "stop") })

			got, err := NewOpenAICompatible(p, "test-key", "", url).Score(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if got.Score != 88 || got.Model != p.Model || !Verify(got.Quote, filing) {
				t.Errorf("got %+v", got)
			}

			req := f.reqs[0]
			rf, _ := req["response_format"].(map[string]any)
			msgs, _ := req["messages"].([]any)
			sys, _ := msgs[0].(map[string]any)
			if req["model"] != p.Model || rf["type"] != "json_object" || !strings.Contains(sys["content"].(string), "json") {
				t.Errorf("request: %v", req)
			}
			if effort, _ := req["reasoning_effort"].(string); effort != p.ReasoningEffort {
				t.Errorf("reasoning_effort = %q, want %q", effort, p.ReasoningEffort)
			}
			if f.auth[0] != "Bearer test-key" {
				t.Errorf("auth %q", f.auth[0])
			}
		})
	}
}

// An endpoint that rejects response_format: drop it, ask again, remember.
func TestJSONModeFallback(t *testing.T) {
	f := &fakeChat{}
	url := f.server(t, func(_ int, req map[string]any) (int, string) {
		if _, ok := req["response_format"]; ok {
			return 400, `{"error":{"message":"Invalid value for response_format","type":"invalid_request_error"}}`
		}
		return 200, completion("gemini-3.5-flash-lite", "```json\n"+goodAnswer+"\n```", "stop")
	})
	c := NewOpenAICompatible(Presets["gemini"], "k", "", url)

	for i := range 2 {
		got, err := c.Score(context.Background(), in)
		if err != nil || got.Score != 88 {
			t.Fatalf("call %d: %+v, %v", i+1, got, err)
		}
	}
	// First call: rejected, then retried without JSON mode. Second call: skips it.
	if len(f.reqs) != 3 {
		t.Errorf("%d requests, want 3", len(f.reqs))
	}
}

// Groq's free tier allows few tokens a minute, so its filings are cut short.
func TestPresetCutsLongFilings(t *testing.T) {
	f := &fakeChat{}
	url := f.server(t, func(int, map[string]any) (int, string) { return 200, completion("x", goodAnswer, "stop") })
	long := Input{Company: "Test Co", FormType: "8-K", Text: filing + strings.Repeat(" more text", 10_000)}
	if _, err := NewOpenAICompatible(Presets["groq"], "k", "", url).Score(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	msgs, _ := f.reqs[0]["messages"].([]any)
	user, _ := msgs[1].(map[string]any)
	content := user["content"].(string)
	if len(content) > 21_000 || !strings.Contains(content, "cut after 20000 characters") {
		t.Errorf("user message is %d characters", len(content))
	}
}

func TestOpenAICompatibleErrors(t *testing.T) {
	var perm *PermanentError
	tests := []struct {
		name      string
		status    int
		body      string
		permanent bool
	}{
		{"bad key", 401, `{"error":{"message":"bad key"}}`, true},
		{"out of balance", 402, `{"error":{"message":"Insufficient Balance"}}`, true},
		{"unknown model", 404, `{"error":{"message":"model not found"}}`, true},
		{"rate limited or free quota used up", 429, `{"error":{"message":"RESOURCE_EXHAUSTED"}}`, false},
		{"overloaded", 503, `{"error":{"message":"busy"}}`, false},
		{"cut off", 200, completion("x", `{"score": 5`, "length"), false},
		{"filtered", 200, completion("x", "", "content_filter"), true},
		{"empty answer", 200, completion("x", "  ", "stop"), false},
		{"not JSON", 200, completion("x", "I think it's important.", "stop"), false},
		{"score out of range", 200, completion("x", `{"score": 140, "reason": "x", "quote": "y"}`, "stop"), true},
		{"missing quote", 200, completion("x", `{"score": 40, "reason": "x", "quote": ""}`, "stop"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeChat{}
			url := f.server(t, func(int, map[string]any) (int, string) { return tt.status, tt.body })
			_, err := NewOpenAICompatible(Presets["deepseek"], "k", "", url).Score(context.Background(), in)
			if err == nil {
				t.Fatal("no error")
			}
			if got := errors.As(err, &perm); got != tt.permanent {
				t.Errorf("permanent = %v, want %v (%v)", got, tt.permanent, err)
			}
		})
	}
}
