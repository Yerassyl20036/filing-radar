package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/shared/constant"
)

// DefaultClaudeModel is used for Claude unless TRIAGE_MODEL says otherwise.
const DefaultClaudeModel = "claude-opus-5-5"

// Claude scores filings and drafts notes with the Anthropic API.
type Claude struct {
	client anthropic.Client
	model  string
	// Set by SetPrices; otherwise the price comes from claudePrices.
	prices *[2]float64
}

// claudePrices are list prices in US$ per million tokens (input, output).
var claudePrices = map[string][2]float64{
	"claude-opus-5-5":   {4, 20},
	"claude-sonnet-5-5": {2, 10},
	"claude-haiku-5-5":  {0.10, 0.50},
}

// SetPrices fixes the price for every model this client reports, so
// configuration can override the table. Call it before the first call.
func (c *Claude) SetPrices(inPerM, outPerM float64) {
	c.prices = &[2]float64{inPerM, outPerM}
}

// price is what the model that answered charges. The answer can come from a
// fallback model, and ids may carry a date suffix, hence the prefix match.
// An unknown model is priced as Opus, so the budget errs on the safe side.
func (c *Claude) price(model string) (float64, float64) {
	if c.prices != nil {
		return c.prices[0], c.prices[1]
	}
	for id, p := range claudePrices {
		if strings.HasPrefix(model, id) {
			return p[0], p[1]
		}
	}
	p := claudePrices["claude-opus-5-5"]
	return p[0], p[1]
}

// NewClaude makes a scorer. The SDK doesn't retry: Temporal does, and only
// calls that weren't billed, so one call's full time stays well inside the
// activity's timeout and a cut-off call is never repeated unseen.
func NewClaude(apiKey, model string) *Claude {
	if model == "" {
		model = DefaultClaudeModel
	}
	return &Claude{
		client: anthropic.NewClient(option.WithAPIKey(apiKey), option.WithMaxRetries(0), option.WithRequestTimeout(2*time.Minute)),
		model:  model,
	}
}

// claudeMaxTokens leaves room for the model's thinking as well as the short
// JSON answer.
const claudeMaxTokens = 8000

// Estimate is a call's worst-case cost; see Estimator.
func (c *Claude) Estimate(promptChars int) Usage {
	inPerM, outPerM := c.price(c.model)
	return estimate(promptChars, maxChars, claudeMaxTokens, inPerM, outPerM)
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

// Score asks the model about one filing. On an error after the API
// answered, the returned Score still carries Usage: the call was billed.
func (c *Claude) Score(ctx context.Context, in Input) (Score, error) {
	r, err := c.call(ctx, system, userPrompt(in, maxChars), schema)
	if err != nil {
		return Score{Usage: r.usage}, err
	}
	out, err := parseAnswer(r.content, r.model)
	out.Usage = r.usage
	return out, err
}

// call sends one message with the answer held to a JSON schema, and returns
// the answer's text.
func (c *Claude) call(ctx context.Context, sys, user string, format map[string]any) (reply, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: claudeMaxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: sys}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user))},
		OutputConfig: anthropic.BetaOutputConfigParam{
			// Classification and short notes gain little from deep thought;
			// low effort keeps each call quick and cheap.
			Effort: anthropic.BetaOutputConfigEffortLow,
			Format: anthropic.BetaJSONOutputFormatParam{Schema: format},
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
			switch {
			case apierr.StatusCode == 401, apierr.StatusCode == 402, apierr.StatusCode == 403,
				apierr.StatusCode == 400 && strings.Contains(strings.ToLower(err.Error()), "credit balance"):
				return reply{}, &PermanentError{&AccountError{fmt.Errorf("model request rejected (%d): %w", apierr.StatusCode, err)}}
			case apierr.StatusCode == 400, apierr.StatusCode == 404:
				return reply{}, &PermanentError{fmt.Errorf("model request rejected (%d): %w", apierr.StatusCode, err)}
			}
		}
		return reply{}, fmt.Errorf("model request: %w", err) // 429, 5xx, network: retry
	}

	r := reply{model: string(resp.Model)}
	r.usage = Usage{InputTokens: int(resp.Usage.InputTokens), OutputTokens: int(resp.Usage.OutputTokens)}
	inPerM, outPerM := c.price(r.model)
	r.usage.CostUSD = Cost(r.usage, inPerM, outPerM)

	switch resp.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return r, &PermanentError{errors.New("the model declined to answer")}
	case anthropic.BetaStopReasonMaxTokens:
		return r, errors.New("the model ran out of room before answering")
	}
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			r.content = t.Text
			return r, nil
		}
	}
	return r, errors.New("model answer has no text")
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
