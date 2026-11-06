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
	// List prices in US$ per million tokens, for the daily budget.
	InputPerM, OutputPerM float64
}

// Presets are the providers the worker knows by name.
var Presets = map[string]Preset{
	// "deepseek-chat" now answers as deepseek-flash. These are its peak
	// prices; off-peak is half, so the budget errs on the safe side.
	"deepseek": {Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-chat", MaxChars: maxChars, InputPerM: 0.30, OutputPerM: 1.20},
	// Free tier through Google AI Studio. Flash-Lite thinks minimally by
	// default, which suits a short JSON answer.
	"gemini": {Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Model: "gemini-3.5-flash-lite", ReasoningEffort: "minimal", MaxChars: maxChars},
	// Free tier: about 200,000 tokens a day, so roughly 30-40 filings; set
	// TRIAGE_MODEL_DAILY_CAP to match.
	"groq": {Name: "groq", BaseURL: "https://api.groq.com/openai/v1", Model: "openai/gpt-oss-120b", ReasoningEffort: "low", MaxChars: 20_000},
}

// OpenAICompatible scores filings and drafts notes through any chat
// completions API. No one
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
	// US$ per million tokens; from the preset unless SetPrices says otherwise.
	inPerM, outPerM float64
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
		inPerM:  p.InputPerM,
		outPerM: p.OutputPerM,
	}
}

// SetPrices replaces the preset's prices, so configuration can follow a
// provider's price change without a release. Call it before the first call.
func (c *OpenAICompatible) SetPrices(inPerM, outPerM float64) {
	c.inPerM, c.outPerM = inPerM, outPerM
}

// maxOutputTokens is a few lines of JSON, plus room for a thinking model's
// reasoning.
const maxOutputTokens = 4096

// Estimate is a call's worst-case cost; see Estimator.
func (c *OpenAICompatible) Estimate(promptChars int) Usage {
	return estimate(promptChars, c.preset.MaxChars, maxOutputTokens, c.inPerM, c.outPerM)
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
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Score asks the model about one filing. On an error after the provider
// answered, the returned Score still carries Usage: the call was billed.
func (c *OpenAICompatible) Score(ctx context.Context, in Input) (Score, error) {
	r, err := c.chat(ctx, system+jsonShape, userPrompt(in, c.preset.MaxChars))
	if err != nil {
		return Score{Usage: r.usage}, err
	}
	out, err := parseAnswer(r.content, r.model)
	out.Usage = r.usage
	return out, err
}

// reply is one chat answer, with what it cost.
type reply struct {
	content string
	model   string
	usage   Usage
}

// chat sends one system and user message in JSON mode and returns the
// answer's text. Usage is filled whenever the provider reported it, even
// when the answer is then unusable.
func (c *OpenAICompatible) chat(ctx context.Context, sys, user string) (reply, error) {
	req := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
		},
		ReasoningEffort: c.preset.ReasoningEffort,
		MaxTokens:       maxOutputTokens,
		Temperature:     0, // the same filing should get the same answer
	}
	if !c.noJSONMode.Load() {
		req.ResponseFormat = &responseFormat{Type: "json_object"}
	}

	status, raw, err := c.post(ctx, req)
	if err != nil {
		return reply{}, err
	}
	// Some endpoints don't take response_format. The prompt asks for JSON
	// anyway, so drop it, remember, and ask once more.
	if status == http.StatusBadRequest && req.ResponseFormat != nil && strings.Contains(strings.ToLower(string(raw)), "response_format") {
		c.noJSONMode.Store(true)
		req.ResponseFormat = nil
		if status, raw, err = c.post(ctx, req); err != nil {
			return reply{}, err
		}
	}

	switch {
	case status == http.StatusOK:
	// 400 bad request, 401 bad key, 402 out of balance (DeepSeek), 403 no
	// access, 404 unknown model, 422 bad parameters: asking again changes
	// nothing.
	case status == 401, status == 402, status == 403:
		return reply{}, &PermanentError{&AccountError{fmt.Errorf("%s rejected the request (%d): %s", c.preset.Name, status, snippet(raw))}}
	case status == 400, status == 404, status == 422:
		return reply{}, &PermanentError{fmt.Errorf("%s rejected the request (%d): %s", c.preset.Name, status, snippet(raw))}
	default: // 429 (rate limit or a used-up free quota), 5xx: retry later
		return reply{}, fmt.Errorf("%s request failed (%d): %s", c.preset.Name, status, snippet(raw))
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil || len(cr.Choices) == 0 {
		return reply{}, fmt.Errorf("%s response isn't a chat completion: %s", c.preset.Name, snippet(raw))
	}
	r := reply{content: cr.Choices[0].Message.Content, model: cr.Model}
	if r.model == "" {
		r.model = c.model
	}
	r.usage = Usage{InputTokens: cr.Usage.PromptTokens, OutputTokens: cr.Usage.CompletionTokens}
	r.usage.CostUSD = Cost(r.usage, c.inPerM, c.outPerM)

	switch cr.Choices[0].FinishReason {
	case "length":
		return r, errors.New("the model ran out of room before answering")
	case "content_filter":
		return r, &PermanentError{errors.New("the model declined to answer")}
	}
	// JSON mode can, rarely, return an empty answer; another try usually works.
	if strings.TrimSpace(r.content) == "" {
		return r, errors.New("the model returned an empty answer")
	}
	return r, nil
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
