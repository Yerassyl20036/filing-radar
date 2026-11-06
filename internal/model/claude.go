package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/shared/constant"
)

// DefaultClaudeModel is used for Claude unless TRIAGE_MODEL says otherwise.
const DefaultClaudeModel = "claude-opus-5-5"

// Claude scores filings with the Anthropic API.
type Claude struct {
	client anthropic.Client
	model  string
}

// NewClaude makes a scorer. The SDK retries 429s and 5xx twice on its own;
// Temporal retries the whole activity on top of that.
func NewClaude(apiKey, model string) *Claude {
	if model == "" {
		model = DefaultClaudeModel
	}
	return &Claude{
		client: anthropic.NewClient(option.WithAPIKey(apiKey), option.WithMaxRetries(2), option.WithRequestTimeout(2*time.Minute)),
		model:  model,
	}
}

// The Claude API enforces this schema on the answer.
var schema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"score":  map[string]any{"type": "integer", "description": "0 to 100, per the scale in the instructions"},
		"reason": map[string]any{"type": "string", "description": "One sentence explaining the score"},
		"quote":  map[string]any{"type": "string", "description": "A short passage copied exactly from the filing"},
	},
	"required":             []string{"score", "reason", "quote"},
	"additionalProperties": false,
}

// Score asks the model about one filing.
func (c *Claude) Score(ctx context.Context, in Input) (Score, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 8000, // room for the model's thinking as well as the short JSON answer
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(userPrompt(in, maxChars)))},
		OutputConfig: anthropic.BetaOutputConfigParam{
			// Classification gains little from deep thought; low effort keeps
			// each call quick and cheap.
			Effort: anthropic.BetaOutputConfigEffortLow,
			Format: anthropic.BetaJSONOutputFormatParam{Schema: schema},
		},
	}
	if hasServerFallback(c.model) {
		// If a safety classifier declines, the API retries on a suitable
		// model inside the same call instead of just stopping.
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}

	resp, err := c.client.Beta.Messages.New(ctx, params)
	if err != nil {
		var apierr *anthropic.Error
		if errors.As(err, &apierr) {
			switch apierr.StatusCode {
			case 400, 401, 403, 404:
				return Score{}, &PermanentError{fmt.Errorf("model request rejected (%d): %w", apierr.StatusCode, err)}
			}
		}
		return Score{}, fmt.Errorf("model request: %w", err) // 429, 5xx, network: retry
	}

	switch resp.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return Score{}, &PermanentError{errors.New("the model declined to score this filing")}
	case anthropic.BetaStopReasonMaxTokens:
		return Score{}, errors.New("the model ran out of room before answering")
	}
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			return parseAnswer(t.Text, string(resp.Model))
		}
	}
	return Score{}, errors.New("model answer has no text")
}

// hasServerFallback: the API's "default" refusal fallback is available for
// these models on the Claude API. Haiku has none.
func hasServerFallback(model string) bool {
	switch model {
	case "claude-opus-5-5", "claude-opus-5", "claude-fable-5-1":
		return true
	}
	return false
}
