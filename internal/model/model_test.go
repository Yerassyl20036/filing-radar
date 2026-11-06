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

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

const filing = `Item 4.02 Non-Reliance on Previously Issued Financial Statements.
On November 4, 2026, the Audit Committee concluded that the Company’s financial statements for fiscal 2025 should no longer be relied upon.`

func TestVerify(t *testing.T) {
	tests := []struct {
		quote string
		want  bool
	}{
		{"the Audit Committee concluded that the Company's financial statements", true}, // straight vs curly apostrophe
		{"THE AUDIT COMMITTEE   concluded that", true},                                  // case and spacing
		{"should no longer be relied upon.", true},                                      // trailing punctuation
		{"the Audit Committee decided the statements were wrong", false},                // a paraphrase
		{"the Company", false}, // too short to prove anything
		{"Non-Reliance on Previously Issued Financial Statements. On November 4, 2026,", true}, // across a line break
	}
	for _, tt := range tests {
		if got := Verify(tt.quote, filing); got != tt.want {
			t.Errorf("Verify(%q) = %v, want %v", tt.quote, got, tt.want)
		}
	}
}

// A stand-in for the Messages API, so the request we send and the way we read
// the answer are tested without a key or a bill.
func fakeAPI(t *testing.T, status int, answer string, stop string) (*Claude, *map[string]any) {
	t.Helper()
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &sent)
		w.Header().Set("Content-Type", "application/json")
		if status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`))
			return
		}
		resp := map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
			"stop_reason": stop,
			"content":     []map[string]any{{"type": "text", "text": answer}},
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 10},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	c := &Claude{
		client: anthropic.NewClient(option.WithAPIKey("test"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0)),
		model:  DefaultClaudeModel,
	}
	return c, &sent
}

func TestScore(t *testing.T) {
	c, sent := fakeAPI(t, 200, `{"score": 88, "reason": "The company says its 2025 statements can't be relied on.", "quote": "should no longer be relied upon"}`, "end_turn")
	got, err := c.Score(context.Background(), Input{Company: "Test Co", FormType: "8-K", Items: []string{"4.02"}, Text: filing})
	if err != nil {
		t.Fatal(err)
	}
	if got.Score != 88 || got.Model != "claude-opus-5-5" || !Verify(got.Quote, filing) {
		t.Errorf("got %+v", got)
	}

	// The request asks for JSON matching the schema, at low effort, with the
	// refusal fallback on.
	oc, _ := (*sent)["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	if oc["effort"] != "low" || format["type"] != "json_schema" || (*sent)["fallbacks"] != "default" {
		t.Errorf("request: output_config=%v fallbacks=%v", oc, (*sent)["fallbacks"])
	}
	msgs, _ := (*sent)["messages"].([]any)
	if b, _ := json.Marshal(msgs); !strings.Contains(string(b), "Items: 4.02") {
		t.Errorf("the filing's items weren't sent: %s", b)
	}
}

func TestScoreErrors(t *testing.T) {
	ctx := context.Background()
	in := Input{Company: "Test Co", FormType: "8-K", Text: filing}
	var perm *PermanentError

	c, _ := fakeAPI(t, 400, "", "")
	if _, err := c.Score(ctx, in); !errors.As(err, &perm) {
		t.Errorf("400 should be permanent: %v", err)
	}
	c, _ = fakeAPI(t, 200, "", "refusal")
	if _, err := c.Score(ctx, in); !errors.As(err, &perm) {
		t.Errorf("a refusal should be permanent: %v", err)
	}
	c, _ = fakeAPI(t, 200, `{"score": 140, "reason": "x", "quote": "y"}`, "end_turn")
	if _, err := c.Score(ctx, in); !errors.As(err, &perm) {
		t.Errorf("a score over 100 should be rejected: %v", err)
	}
	c, _ = fakeAPI(t, 503, "", "")
	if _, err := c.Score(ctx, in); err == nil || errors.As(err, &perm) {
		t.Errorf("503 should be retryable: %v", err)
	}
}
