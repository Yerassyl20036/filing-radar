// Package model asks a language model how material an 8-K is. Each answer is
// a score from 0 to 100, one sentence of reason, and a quote from the filing
// that the caller checks before trusting the score.
//
// The workflow only sees the Scorer interface. DeepSeek and Claude are two
// implementations of it, chosen by configuration; the prompt, the answer's
// shape and the checks are shared, so swapping one for the other changes
// nothing downstream.
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
}

// Scorer is what the workflow calls: DeepSeek, Claude, or a fake in tests.
type Scorer interface {
	Score(ctx context.Context, in Input) (Score, error)
}

// PermanentError means asking again won't help: a bad key, a request the API
// rejects, or a refusal. Everything else (rate limits, overload, timeouts) is
// worth a retry.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

const system = `You triage SEC filings for a public feed of company news.

Read the 8-K and score how material it is to the company's stock price, from 0 to 100:
- 0 to 14: paperwork with no news (bylaw edits, vote results, routine exhibits).
- 15 to 39: minor news unlikely to move the price.
- 40 to 69: notable news a shareholder would want to know (a new executive, new debt, earnings in line with expectations).
- 70 to 100: major news likely to move the price (a restatement, bankruptcy, merger, delisting, an earnings surprise, a large cyber incident).

Base the score only on what the filing says. Write the reason as one plain sentence a reader can check against the filing. For the quote, copy one short passage (under 40 words) word for word from the filing that best supports the score; don't paraphrase or join separate sentences.`

// userPrompt is the filing as the model sees it.
func userPrompt(in Input) string {
	text := in.Text
	note := ""
	if len(text) > maxChars {
		text = strings.ToValidUTF8(text[:maxChars], "") // don't split a character
		note = fmt.Sprintf("\n(The document was cut after %d characters.)", maxChars)
	}
	return fmt.Sprintf("Company: %s\nForm: %s\nItems: %s\n\n<filing>\n%s\n</filing>%s",
		in.Company, in.FormType, strings.Join(in.Items, ", "), text, note)
}

// parseAnswer reads the model's JSON and checks what a schema can't express.
func parseAnswer(text, model string) (Score, error) {
	var out Score
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &out); err != nil {
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
