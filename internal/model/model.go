// Package model asks a Claude model how material an 8-K is. Each answer is a
// score from 0 to 100, one sentence of reason, and a quote from the filing
// that the caller checks before trusting the score.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/shared/constant"
)

// DefaultModel is used unless TRIAGE_MODEL says otherwise.
const DefaultModel = "claude-opus-5-5"

// maxChars bounds what one call can cost. 8-Ks are usually a few thousand
// words; the rare giant one is cut here and the prompt says so.
const maxChars = 150_000

// Input is one filing to score.
type Input struct {
	Company  string
	FormType string
	Items    []string // 8-K item numbers, as SEC lists them
	Text     string   // the main document as plain text
}

// Score is the model's answer.
type Score struct {
	Score  int    `json:"score"`
	Reason string `json:"reason"`
	Quote  string `json:"quote"`
	Model  string `json:"-"` // which model answered (a fallback may stand in)
}

// Scorer is what the workflow calls: Claude, or a fake in tests.
type Scorer interface {
	Score(ctx context.Context, in Input) (Score, error)
}

// PermanentError means asking again won't help: a bad key, a request the API
// rejects, or a refusal. Everything else (rate limits, overload, timeouts) is
// worth a retry.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Claude scores filings with the Anthropic API.
type Claude struct {
	client anthropic.Client
	model  string
}

// NewClaude makes a scorer. The SDK retries 429s and 5xx twice on its own;
// Temporal retries the whole activity on top of that.
func NewClaude(apiKey, model string) *Claude {
	if model == "" {
		model = DefaultModel
	}
	return &Claude{
		client: anthropic.NewClient(option.WithAPIKey(apiKey), option.WithMaxRetries(2), option.WithRequestTimeout(2*time.Minute)),
		model:  model,
	}
}

const system = `You triage SEC filings for a public feed of company news.

Read the 8-K and score how material it is to the company's stock price, from 0 to 100:
- 0 to 14: paperwork with no news (bylaw edits, vote results, routine exhibits).
- 15 to 39: minor news unlikely to move the price.
- 40 to 69: notable news a shareholder would want to know (a new executive, new debt, earnings in line with expectations).
- 70 to 100: major news likely to move the price (a restatement, bankruptcy, merger, delisting, an earnings surprise, a large cyber incident).

Base the score only on what the filing says. Write the reason as one plain sentence a reader can check against the filing. For the quote, copy one short passage (under 40 words) word for word from the filing that best supports the score; don't paraphrase or join separate sentences.`

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
	text := in.Text
	note := ""
	if len(text) > maxChars {
		text = strings.ToValidUTF8(text[:maxChars], "") // don't split a character
		note = fmt.Sprintf("\n(The document was cut after %d characters.)", maxChars)
	}
	user := fmt.Sprintf("Company: %s\nForm: %s\nItems: %s\n\n<filing>\n%s\n</filing>%s",
		in.Company, in.FormType, strings.Join(in.Items, ", "), text, note)

	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 8000, // room for the model's thinking as well as the short JSON answer
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user))},
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

	var out Score
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			if err := json.Unmarshal([]byte(t.Text), &out); err != nil {
				return Score{}, fmt.Errorf("model answer isn't the expected JSON: %w", err)
			}
			out.Model = string(resp.Model)
			return out, validate(out)
		}
	}
	return Score{}, errors.New("model answer has no text")
}

// validate checks what the schema can't express.
func validate(s Score) error {
	if s.Score < 0 || s.Score > 100 {
		return &PermanentError{fmt.Errorf("score %d is outside 0 to 100", s.Score)}
	}
	if strings.TrimSpace(s.Reason) == "" || strings.TrimSpace(s.Quote) == "" {
		return &PermanentError{errors.New("the model left the reason or quote empty")}
	}
	return nil
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
