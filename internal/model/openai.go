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
	"sync/atomic"
	"time"
)

// Preset is one provider that speaks OpenAI's chat completions format.
// DeepSeek, Gemini and Groq all do, so one client serves them all; only the
// address, the default model and a few limits differ.
type Preset struct {
	Name    string
	BaseURL string // {BaseURL}/chat/completions
	Model   string // the default model
	// ReasoningEffort, when set, keeps a thinking model's reasoning short.
	// On Gemini, thinking tokens count against max_tokens, and a long think
	// can leave no room for the answer.
	ReasoningEffort string
	// MaxChars caps the filing text. Free tiers limit tokens per minute;
	// Groq's free tier allows about 8,000.
	MaxChars int
}

// Presets are the providers the worker knows by name.
var Presets = map[string]Preset{
	"deepseek": {Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-chat", MaxChars: maxChars},
	// Free tier through Google AI Studio. Flash-Lite thinks minimally by
	// default, which suits a short JSON answer.
	"gemini": {Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Model: "gemini-3.5-flash-lite", ReasoningEffort: "minimal", MaxChars: maxChars},
	// Free tier: about 200,000 tokens a day, so roughly 30-40 filings; set
	// TRIAGE_MODEL_DAILY_CAP to match.
	"groq": {Name: "groq", BaseURL: "https://api.groq.com/openai/v1", Model: "openai/gpt-oss-120b", ReasoningEffort: "low", MaxChars: 20_000},
}

// OpenAICompatible scores filings through any chat completions API. No one
// SDK covers all three, and the call is one small JSON request, so plain
// net/http it is.
type OpenAICompatible struct {
	preset  Preset
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
	// Set after a provider rejects JSON mode, so later calls skip it.
	noJSONMode atomic.Bool
}

// NewOpenAICompatible makes a scorer from a preset. model and baseURL
// override the preset's when not empty (baseURL is for tests).
func NewOpenAICompatible(p Preset, apiKey, model, baseURL string) *OpenAICompatible {
	if model == "" {
		model = p.Model
	}
	if baseURL == "" {
		baseURL = p.BaseURL
	}
	if p.MaxChars == 0 {
		p.MaxChars = maxChars
	}
	return &OpenAICompatible{
		preset:  p,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Timeout: 2 * time.Minute},
	}
}

// JSON mode, where offered, guarantees valid JSON but not a schema. So the
// prompt spells out the shape (it must also contain the word "json"), and
// parseAnswer checks every field anyway.
const jsonShape = `

Answer in json only, in exactly this shape:
{"score": <integer 0 to 100>, "reason": "<one sentence>", "quote": "<passage copied from the filing>"}`

type chatRequest struct {
	Model           string          `json:"model"`
	Messages        []chatMessage   `json:"messages"`
	ResponseFormat  *responseFormat `json:"response_format,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	MaxTokens       int             `json:"max_tokens"`
	Temperature     float64         `json:"temperature"`
	Stream          bool            `json:"stream"`
}

type responseFormat struct {
	Type string `json:"type"`
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
func (c *OpenAICompatible) Score(ctx context.Context, in Input) (Score, error) {
	req := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: system + jsonShape},
			{Role: "user", Content: userPrompt(in, c.preset.MaxChars)},
		},
		ReasoningEffort: c.preset.ReasoningEffort,
		MaxTokens:       4096, // a few lines of JSON, plus room for a thinking model's reasoning
		Temperature:     0,    // the same filing should get the same score
	}
	if !c.noJSONMode.Load() {
		req.ResponseFormat = &responseFormat{Type: "json_object"}
	}

	status, raw, err := c.post(ctx, req)
	if err != nil {
		return Score{}, err
	}
	// Some endpoints don't take response_format. The prompt asks for JSON
	// anyway, so drop it, remember, and ask once more.
	if status == http.StatusBadRequest && req.ResponseFormat != nil && strings.Contains(strings.ToLower(string(raw)), "response_format") {
		c.noJSONMode.Store(true)
		req.ResponseFormat = nil
		if status, raw, err = c.post(ctx, req); err != nil {
			return Score{}, err
		}
	}

	switch {
	case status == http.StatusOK:
	// 400 bad request, 401 bad key, 402 out of balance (DeepSeek), 403 no
	// access, 404 unknown model, 422 bad parameters: asking again changes
	// nothing.
	case status == 400, status == 401, status == 402, status == 403, status == 404, status == 422:
		return Score{}, &PermanentError{fmt.Errorf("%s rejected the request (%d): %s", c.preset.Name, status, snippet(raw))}
	default: // 429 (rate limit or a used-up free quota), 5xx: retry later
		return Score{}, fmt.Errorf("%s request failed (%d): %s", c.preset.Name, status, snippet(raw))
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil || len(cr.Choices) == 0 {
		return Score{}, fmt.Errorf("%s response isn't a chat completion: %s", c.preset.Name, snippet(raw))
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
		model = c.model
	}
	return parseAnswer(choice.Message.Content, model)
}

// post sends one request and returns the status and body. Only a transport
// failure is an error here; the caller decides what each status means.
func (c *OpenAICompatible) post(ctx context.Context, req chatRequest) (int, []byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 0, nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return 0, nil, fmt.Errorf("%s request: %w", c.preset.Name, err) // network: retry
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%s response: %w", c.preset.Name, err)
	}
	return resp.StatusCode, raw, nil
}

// snippet keeps error messages short; API errors are small JSON objects.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
