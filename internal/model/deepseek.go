package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultDeepSeekModel is DeepSeek's general chat model.
	DefaultDeepSeekModel = "deepseek-chat"
	// DefaultDeepSeekURL is DeepSeek's API. It follows OpenAI's chat format.
	DefaultDeepSeekURL = "https://api.deepseek.com"
)

// DeepSeek scores filings with DeepSeek's chat completions API. There's no
// official Go SDK, and the API is one small JSON request, so plain net/http
// it is.
type DeepSeek struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// NewDeepSeek makes a scorer. baseURL is DefaultDeepSeekURL unless testing.
func NewDeepSeek(apiKey, model, baseURL string) *DeepSeek {
	if model == "" {
		model = DefaultDeepSeekModel
	}
	if baseURL == "" {
		baseURL = DefaultDeepSeekURL
	}
	return &DeepSeek{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Timeout: 2 * time.Minute},
	}
}

// DeepSeek's JSON mode guarantees valid JSON but not a schema, so the prompt
// spells out the shape (it must also contain the word "json"), and
// parseAnswer checks every field anyway.
const deepSeekShape = `

Answer in json only, in exactly this shape:
{"score": <integer 0 to 100>, "reason": "<one sentence>", "quote": "<passage copied from the filing>"}`

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	Stream      bool    `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
}

// Score asks the model about one filing.
func (d *DeepSeek) Score(ctx context.Context, in Input) (Score, error) {
	req := chatRequest{
		Model: d.model,
		Messages: []chatMessage{
			{Role: "system", Content: system + deepSeekShape},
			{Role: "user", Content: userPrompt(in)},
		},
		MaxTokens:   1024, // the answer is a few lines of JSON
		Temperature: 0,    // the same filing should get the same score
	}
	req.ResponseFormat.Type = "json_object"
	body, err := json.Marshal(req)
	if err != nil {
		return Score{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Score{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+d.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := d.http.Do(httpReq)
	if err != nil {
		return Score{}, fmt.Errorf("model request: %w", err) // network: retry
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Score{}, fmt.Errorf("model response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
	// 400 bad format, 401 bad key, 402 out of balance, 422 bad parameters:
	// asking again changes nothing.
	case resp.StatusCode == 400, resp.StatusCode == 401, resp.StatusCode == 402, resp.StatusCode == 422:
		return Score{}, &PermanentError{fmt.Errorf("model request rejected (%d): %s", resp.StatusCode, snippet(raw))}
	default: // 429 rate limit, 500, 503 overloaded: retry
		return Score{}, fmt.Errorf("model request failed (%d): %s", resp.StatusCode, snippet(raw))
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil || len(cr.Choices) == 0 {
		return Score{}, fmt.Errorf("model response isn't a chat completion: %s", snippet(raw))
	}
	choice := cr.Choices[0]
	switch choice.FinishReason {
	case "length":
		return Score{}, errors.New("the model ran out of room before answering")
	case "content_filter":
		return Score{}, &PermanentError{errors.New("the model declined to score this filing")}
	}
	// JSON mode can, rarely, return an empty answer; another try usually works.
	if strings.TrimSpace(choice.Message.Content) == "" {
		return Score{}, errors.New("the model returned an empty answer")
	}
	model := cr.Model
	if model == "" {
		model = d.model
	}
	return parseAnswer(choice.Message.Content, model)
}

// snippet keeps error messages short; API errors are small JSON objects.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
