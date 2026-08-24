// Package notify sends Coldarr's Apprise webhook notifications - a plain
// JSON POST to a user-configured Apprise (https://github.com/caronc/apprise)
// endpoint, following the Apprise API's standard notify payload shape.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Level is the Apprise notification type - Apprise uses this to choose an
// icon/color in whatever client renders the notification.
type Level string

const (
	LevelInfo    Level = "info"
	LevelSuccess Level = "success"
	LevelWarning Level = "warning"
	LevelFailure Level = "failure"
)

type payload struct {
	Title  string `json:"title,omitempty"`
	Body   string `json:"body"`
	Type   string `json:"type"`
	Tag    string `json:"tag,omitempty"`
	Format string `json:"format,omitempty"`
}

// Notifier sends Apprise webhook notifications for one configured URL. A
// nil *Notifier, or one with an empty URL, is a safe no-op on Summary and
// Item - callers never need to check "is this configured" themselves.
type Notifier struct {
	URL     string
	Verbose bool
	// Markdown tells Apprise ("format": "markdown" in the outgoing
	// payload) to interpret Body as Markdown rather than plain text, and
	// makes Bold/Code/JoinLines actually apply Markdown syntax instead of
	// passing text through unchanged. Markdown messages also put their
	// action in a formatted body header rather than Apprise's title field.
	Markdown bool
	// Tag restricts delivery to the Apprise notification target(s)
	// registered under this tag on the receiving end - many Apprise API
	// deployments route entirely by tag and return HTTP 424 ("failed
	// dependency") if a request matches none, most often because no tag
	// was sent at all. Left blank, no "tag" field is sent (Apprise's own
	// default routing applies).
	Tag string
}

// markdownEscaper neutralizes characters Markdown would otherwise parse as
// emphasis delimiters inside a value about to be wrapped in **bold** -
// without it, a tier/label containing "_" (e.g. "temphdd_path2") could
// unintentionally start an italic span instead of rendering literally.
var markdownEscaper = strings.NewReplacer(
	`\`, `\\`,
	`*`, `\*`,
	`_`, `\_`,
	`~`, `\~`,
)

// Bold wraps s in Markdown emphasis when Markdown is enabled (escaping any
// emphasis-like characters s already contains), and returns s unchanged
// otherwise. Meant for short label-like values (tier names, counts,
// percentages) - a nil Notifier behaves as Markdown-off.
func (n *Notifier) Bold(s string) string {
	if n == nil || !n.Markdown {
		return s
	}
	return "**" + markdownEscaper.Replace(s) + "**"
}

// Code wraps s in a Markdown code span when Markdown is enabled, and
// returns s unchanged otherwise. Meant for arbitrary/identifier-like
// values (paths, item titles, raw error text) - a code span is never
// parsed for emphasis, so unlike Bold it needs no escaping.
func (n *Notifier) Code(s string) string {
	if n == nil || !n.Markdown {
		return s
	}
	return "`" + s + "`"
}

// JoinLines joins lines with newlines under Markdown and "; " otherwise,
// unchanged from before Markdown existed.
func (n *Notifier) JoinLines(lines []string) string {
	sep := "; "
	if n != nil && n.Markdown {
		sep = "\n"
	}
	return strings.Join(lines, sep)
}

// Summary always sends when a URL is configured - the low-noise default
// every task sends exactly once per run, regardless of Verbose.
func (n *Notifier) Summary(title, body string, lvl Level) {
	n.send(title, body, lvl)
}

// Item only actually sends when Verbose is enabled, so call sites never
// need to check the flag themselves.
func (n *Notifier) Item(title, body string, lvl Level) {
	if n == nil || !n.Verbose {
		return
	}
	n.send(title, body, lvl)
}

// send logs and swallows any error - a failed notification must never
// fail the task that triggered it.
func (n *Notifier) send(title, body string, lvl Level) {
	if n == nil || n.URL == "" {
		return
	}
	if err := post(n.URL, n.Tag, title, body, lvl, n.Markdown); err != nil {
		log.Printf("notify: sending to apprise failed: %v", err)
	}
}

// Test sends one notification to url (optionally restricted to tag) and
// returns any error directly - unlike Summary/Item, which log-and-swallow,
// Test exists specifically to show an operator whether their configured
// URL (and tag, if their Apprise setup routes by one) actually works. When
// markdown is true, the body demonstrates Bold/Code so the operator can
// see whether their Apprise target actually renders Markdown rather than
// showing literal asterisks/backticks.
func Test(url, tag string, markdown bool) error {
	body := "If you can see this, Coldarr can reach your Apprise endpoint."
	if markdown {
		body = "If you can reach your Apprise endpoint, and **this** is bold and `this` is code (not literal asterisks/backticks), Markdown formatting is working."
	}
	return post(url, tag, "Coldarr test notification", body, LevelInfo, markdown)
}

func post(url, tag, title, body string, lvl Level, markdown bool) error {
	p := payload{Title: title, Body: body, Type: string(lvl), Tag: tag}
	if markdown {
		p.Title = ""
		p.Body = fmt.Sprintf("❄️`Coldarr` *%s*:\n%s", markdownEscaper.Replace(title), body)
		p.Format = "markdown"
	}
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encoding notification: %w", err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: unexpected status %d", url, resp.StatusCode)
	}
	return nil
}
