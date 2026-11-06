// Package model asks a language model how material an 8-K is. Each answer is
// a score from 0 to 100, one sentence of reason, and a quote from the filing
// that the caller checks before trusting the score.
//
// The workflow only sees the Scorer interface. Claude is one implementation;
// OpenAICompatible is the other, covering DeepSeek, Gemini and Groq through
// presets. The prompt, the answer's shape and the checks are shared, so
// swapping providers changes nothing downstream.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

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
	Usage  Usage  `json:"-"`
}

// Usage is what one call consumed, as the API reported it, and what that
// cost at the provider's list price.
type Usage struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// Scorer is what the workflow calls: a real provider, or a fake in tests.
type Scorer interface {
	Score(ctx context.Context, in Input) (Score, error)
}

// PermanentError means asking again won't help: a bad key, a request the API
// rejects, or a refusal. Everything else (rate limits, overload, timeouts) is
// worth a retry.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// AccountError is a rejection about the account, not the request: no
// balance left, a bad key, no access. It comes wrapped in a PermanentError,
// since asking again soon won't help, but once the account is fixed the
// same request will work, so callers shouldn't give up on the work itself.
type AccountError struct{ Err error }

func (e *AccountError) Error() string { return e.Err.Error() }
func (e *AccountError) Unwrap() error { return e.Err }

// Estimator prices a call before it is made, so a daily budget can count
// calls still in flight. It errs high: about three characters a token, and
// the most output a call may produce.
type Estimator interface {
	Estimate(promptChars int) Usage
}

// promptOverhead is what a prompt adds around the documents: instructions,
// the header and, for a repair, the earlier draft.
const promptOverhead = 8_000

// estimate is the worst case for a prompt of chars characters, capped at
// what the client would send.
func estimate(chars, maxChars, maxOutput int, inPerM, outPerM float64) Usage {
	u := Usage{InputTokens: (min(chars, maxChars)+promptOverhead)/3 + 1, OutputTokens: maxOutput}
	u.CostUSD = Cost(u, inPerM, outPerM)
	return u
}

const system = `You triage SEC filings for a public feed of company news.

Read the 8-K and score how material it is to the company's stock price, from 0 to 100:
- 0 to 14: paperwork with no news (bylaw edits, vote results, routine exhibits).
- 15 to 39: minor news unlikely to move the price.
- 40 to 69: notable news a shareholder would want to know (a new executive, new debt, earnings in line with expectations).
- 70 to 100: major news likely to move the price (a restatement, bankruptcy, merger, delisting, an earnings surprise, a large cyber incident).

Base the score only on what the filing says. Write the reason as one plain sentence a reader can check against the filing. For the quote, copy one short passage (under 40 words) word for word from the filing that best supports the score; don't paraphrase or join separate sentences.`

// userPrompt is the filing as the model sees it, cut at limit characters.
func userPrompt(in Input, limit int) string {
	text := in.Text
	note := ""
	if len(text) > limit {
		text = strings.ToValidUTF8(text[:limit], "") // don't split a character
		note = fmt.Sprintf("\n(The document was cut after %d characters.)", limit)
	}
	return fmt.Sprintf("Company: %s\nForm: %s\nItems: %s\n\n<filing>\n%s\n</filing>%s",
		in.Company, in.FormType, strings.Join(in.Items, ", "), text, note)
}

// parseAnswer reads the model's JSON and checks what a schema can't express.
// Without JSON mode some models wrap the answer in a ```json fence or add a
// sentence around it, so it reads the outermost {...}.
func parseAnswer(text, model string) (Score, error) {
	text = strings.TrimSpace(text)
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		text = text[i : j+1]
	}
	var out Score
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return Score{}, fmt.Errorf("model answer isn't the expected JSON: %w", err)
	}
	out.Model = model
	if out.Score < 0 || out.Score > 100 {
		return Score{}, &PermanentError{fmt.Errorf("score %d is outside 0 to 100", out.Score)}
	}
	if strings.TrimSpace(out.Reason) == "" || strings.TrimSpace(out.Quote) == "" {
		return Score{}, &PermanentError{errors.New("the model left the reason or quote empty")}
	}
	return out, nil
}

// Cost prices a call at per-million-token list prices. Providers bill by
// token, so this is the number the daily budget compares against.
func Cost(u Usage, inPerM, outPerM float64) float64 {
	return (float64(u.InputTokens)*inPerM + float64(u.OutputTokens)*outPerM) / 1e6
}
