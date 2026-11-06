package edgar

import (
	"context"
	"fmt"
	"io"
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding/charmap"
)

// Document fetches a filing's main document from sec.gov and returns it as
// readable plain text.
func (c *Client) Document(ctx context.Context, cik, accessionNo, primaryDoc string) (string, error) {
	resp, err := c.fetch(ctx, DocumentURL(c.cfg.BaseURL, cik, accessionNo, primaryDoc), "text/html, text/plain")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	return Text(io.LimitReader(resp.Body, maxBody), resp.Header.Get("Content-Type"))
}

// How far into a document to look for <html> or <body> before deciding it's
// plain text. Inline XBRL has <html> in its first few hundred bytes.
const sniffLen = 8 << 10

// Text turns an HTML or plain-text filing document into readable text.
func Text(r io.Reader, contentType string) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read document: %w", err)
	}
	s, err := decode(b, contentType)
	if err != nil {
		return "", fmt.Errorf("decode document: %w", err)
	}
	var w textWriter
	if isPlain(s, contentType) {
		w.write(s, true)
	} else {
		writeHTML(&w, s)
	}
	return w.b.String(), nil
}

// decode converts b to UTF-8. A charset in the Content-Type or a byte-order
// mark is trusted. Otherwise a document that is valid UTF-8 is UTF-8: the
// charset package looks only at the first 1 KB, which in an inline XBRL file
// is all-ASCII header, and would call it windows-1252. Anything else is read
// as its <meta> says, or as windows-1252, which is what SEC's undeclared
// pages really are; so is a page whose UTF-8 label is plainly wrong.
func decode(b []byte, contentType string) (string, error) {
	enc, name, certain := charset.DetermineEncoding(b, contentType)
	valid := utf8.Valid(b)
	switch {
	case valid && (!certain || name == "utf-8"):
		return string(b), nil
	case !valid && name == "utf-8":
		enc = charmap.Windows1252
	}
	return enc.NewDecoder().String(string(b))
}

// isPlain reports whether s should be read as plain text rather than HTML.
// Old text filings are full of tags like <PAGE> and <TABLE>, so only <html>
// or <body> counts as a sign of HTML.
func isPlain(s, contentType string) bool {
	if mt, _, _ := mime.ParseMediaType(contentType); mt == "text/plain" {
		return true
	}
	head := strings.ToLower(s[:min(len(s), sniffLen)])
	return !strings.Contains(head, "<html") && !strings.Contains(head, "<body")
}

// Elements whose content a reader never sees. true marks those the tokenizer
// reads as raw text up to their end tag even when written <script/>, as
// browsers do, so a self-closed one still has content to skip.
var dropped = map[string]bool{
	"head": false, "ix:header": false,
	"script": true, "style": true, "title": true, "noscript": true,
	"iframe": true, "noembed": true, "noframes": true,
}

// Elements that can't have content, so hiding one hides nothing after it.
var void = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true,
}

// Block elements end the line they're on. The value is how many newlines
// they leave, so paragraphs and tables get a blank line around them.
var blocks = map[string]int{
	"address": 1, "article": 1, "aside": 1, "caption": 1, "center": 1, "dd": 1, "div": 1,
	"dl": 1, "dt": 1, "figure": 1, "footer": 1, "form": 1, "header": 1, "hr": 1, "li": 1,
	"main": 1, "nav": 1, "section": 1, "tr": 1,
	"blockquote": 2, "h1": 2, "h2": 2, "h3": 2, "h4": 2, "h5": 2, "h6": 2,
	"ol": 2, "p": 2, "pre": 2, "table": 2, "ul": 2,
}

// writeHTML writes the text a reader would see. It walks tokens rather than
// building a tree, which is cheaper for big filings; inline XBRL is XHTML, so
// tags balance and counting them is enough to skip a hidden element.
func writeHTML(w *textWriter, s string) {
	z := html.NewTokenizer(strings.NewReader(s))
	var (
		skip   string // the element being dropped, if any
		depth  int    // elements of the same name open inside it
		pre    int    // open <pre> elements, which keep their line breaks
		inCell bool   // in a table cell, where blocks run on as one line
	)
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return // io.EOF: s is all in memory
		case html.TextToken:
			if skip == "" {
				w.write(string(z.Text()), pre > 0)
			}
			continue
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
		default:
			continue // comments and doctypes
		}
		b, hasAttr := z.TagName()
		name := string(b)

		if skip != "" {
			switch {
			case name == skip && tt == html.StartTagToken:
				depth++
			case name == skip && tt == html.EndTagToken && depth > 0:
				depth--
			case name == skip && tt == html.EndTagToken,
				skip == "head" && name == "body": // </head> is optional
				skip = ""
			}
			continue
		}
		raw, drop := dropped[name]
		if drop && (tt == html.StartTagToken || raw) ||
			tt == html.StartTagToken && hasAttr && !void[name] && hidden(z) {
			skip, depth = name, 0
			continue
		}

		switch name {
		case "td", "th":
			if inCell = tt == html.StartTagToken; inCell {
				w.cell()
			}
			continue
		case "br":
			if tt == html.EndTagToken {
				continue
			}
			if inCell {
				w.space()
			} else {
				w.lineBreak()
			}
			continue
		case "tr", "table":
			inCell = false
		case "pre":
			if tt == html.StartTagToken {
				pre++
			} else if tt == html.EndTagToken && pre > 0 {
				pre--
			}
		}
		if n := blocks[name]; n > 0 {
			if inCell {
				w.space()
			} else {
				w.endLine(n)
			}
		}
	}
}

// hidden reports whether the tag just read has an inline style that hides
// it, as inline XBRL does with the <div> around its machine-only facts.
func hidden(z *html.Tokenizer) bool {
	for {
		key, val, more := z.TagAttr()
		if string(key) == "style" {
			style := strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return -1
				}
				return unicode.ToLower(r)
			}, string(val))
			if strings.Contains(style, "display:none") {
				return true
			}
		}
		if !more {
			return false
		}
	}
}

// textWriter builds the output. Line breaks and spaces are owed rather than
// written, and the next character settles them: a visible one pays them,
// another break merges with them, and the end of the document drops them.
// That gives trimmed lines, single spaces and at most one blank line in a row
// without a second pass.
type textWriter struct {
	b      strings.Builder
	breaks int    // newlines owed before the next character, at most 2
	sep    string // owed before the next character on the same line: "", " " or " | "
}

// write adds text. Whitespace runs become one space; newlines end the line
// only where they mean something: plain text and <pre>.
func (w *textWriter) write(s string, keepLines bool) {
	for _, r := range s {
		switch {
		case r == '\n' && keepLines:
			w.lineBreak()
		case r == '\u200b' || r == '\ufeff': // zero-width, left by word processors
		case unicode.IsSpace(r): // includes &nbsp;
			w.space()
		default:
			if w.b.Len() > 0 {
				if w.breaks > 0 {
					w.b.WriteString("\n\n"[:w.breaks])
				} else {
					w.b.WriteString(w.sep)
				}
			}
			w.breaks, w.sep = 0, ""
			w.b.WriteRune(r)
		}
	}
}

// space separates words, unless nothing is on the line yet.
func (w *textWriter) space() {
	if w.breaks == 0 && w.sep == "" {
		w.sep = " "
	}
}

// cell starts a table cell. Only a cell with text in it gets a separator, so
// SEC's empty spacer columns vanish.
func (w *textWriter) cell() {
	if w.breaks == 0 {
		w.sep = " | "
	}
}

// endLine ends the current line, if anything is on it, with n newlines.
func (w *textWriter) endLine(n int) {
	w.breaks, w.sep = max(w.breaks, n), ""
}

// lineBreak is a <br> or a newline in plain text: each one counts, up to a
// single blank line.
func (w *textWriter) lineBreak() {
	w.breaks, w.sep = min(w.breaks+1, 2), ""
}
