package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDeepSeek stands in for the chat completions endpoint.
func fakeDeepSeek(t *testing.T, status int, content, finish string) (*DeepSeek, *map[string]any, *http.Header) {
	t.Helper()
	var sent map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		hdr = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &sent)
		w.Header().Set("Content-Type", "application/json")
		if status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"nope","type":"invalid_request_error"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "model": "deepseek-chat",
			"choices": []map[string]any{{"index": 0, "finish_reason": finish, "message": map[string]any{"role": "assistant", "content": content}}},
		})
	}))
	t.Cleanup(srv.Close)
	return NewDeepSeek("test-key", "", srv.URL), &sent, &hdr
}

func TestDeepSeekScore(t *testing.T) {
	d, sent, hdr := fakeDeepSeek(t, 200, `{"score": 88, "reason": "Past statements can't be relied on.", "quote": "should no longer be relied upon"}`, "stop")
	got, err := d.Score(context.Background(), Input{Company: "Test Co", FormType: "8-K", Items: []string{"4.02"}, Text: filing})
	if err != nil {
		t.Fatal(err)
	}
	if got.Score != 88 || got.Model != "deepseek-chat" || !Verify(got.Quote, filing) {
		t.Errorf("got %+v", got)
	}

	// JSON mode on, the shape spelled out (with the word "json"), the key sent.
	rf, _ := (*sent)["response_format"].(map[string]any)
	msgs, _ := (*sent)["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	if rf["type"] != "json_object" || !strings.Contains(sys["content"].(string), "json") || (*sent)["model"] != "deepseek-chat" {
		t.Errorf("request: %v", *sent)
	}
	if hdr.Get("Authorization") != "Bearer test-key" {
		t.Errorf("auth header %q", hdr.Get("Authorization"))
	}
}

func TestDeepSeekErrors(t *testing.T) {
	ctx := context.Background()
	in := Input{Company: "Test Co", FormType: "8-K", Text: filing}
	var perm *PermanentError
	tests := []struct {
		name      string
		status    int
		content   string
		finish    string
		permanent bool
	}{
		{"bad key", 401, "", "", true},
		{"out of balance", 402, "", "", true},
		{"rate limited", 429, "", "", false},
		{"overloaded", 503, "", "", false},
		{"cut off", 200, `{"score": 5`, "length", false},
		{"filtered", 200, "", "content_filter", true},
		{"empty answer", 200, "  ", "stop", false},
		{"not JSON", 200, "I think it's important.", "stop", false},
		{"score out of range", 200, `{"score": 140, "reason": "x", "quote": "y"}`, "stop", true},
		{"missing quote", 200, `{"score": 40, "reason": "x", "quote": ""}`, "stop", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _, _ := fakeDeepSeek(t, tt.status, tt.content, tt.finish)
			_, err := d.Score(ctx, in)
			if err == nil {
				t.Fatal("no error")
			}
			if got := errors.As(err, &perm); got != tt.permanent {
				t.Errorf("permanent = %v, want %v (%v)", got, tt.permanent, err)
			}
		})
	}
}
