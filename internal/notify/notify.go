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
	Title string `json:"title"`
	Body  string `json:"body"`
	Type  string `json:"type"`
}

// Notifier sends Apprise webhook notifications for one configured URL. A
// nil *Notifier, or one with an empty URL, is a safe no-op on Summary and
// Item - callers never need to check "is this configured" themselves.
type Notifier struct {
	URL     string
	Verbose bool
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
	if err := post(n.URL, title, body, lvl); err != nil {
		log.Printf("notify: sending to apprise failed: %v", err)
	}
}

// Test sends one notification to url and returns any error directly -
// unlike Summary/Item, which log-and-swallow, Test exists specifically to
// show an operator whether their configured URL actually works.
func Test(url string) error {
	return post(url, "Coldarr test notification", "If you can see this, Coldarr can reach your Apprise endpoint.", LevelInfo)
}

func post(url, title, body string, lvl Level) error {
	data, err := json.Marshal(payload{Title: title, Body: body, Type: string(lvl)})
	if err != nil {
		return fmt.Errorf("encoding notification: %w", err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: unexpected status %d", url, resp.StatusCode)
	}
	return nil
}
