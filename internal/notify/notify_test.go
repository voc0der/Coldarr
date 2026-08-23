package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func captureServer(t *testing.T) (*httptest.Server, chan payload) {
	t.Helper()
	received := make(chan payload, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var p payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		received <- p
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, received
}

func TestNotifier_Summary_SendsRegardlessOfVerbose(t *testing.T) {
	srv, received := captureServer(t)

	n := &Notifier{URL: srv.URL, Verbose: false}
	n.Summary("Apply finished", "moved 3, failed 0", LevelSuccess)

	select {
	case p := <-received:
		if p.Title != "Apply finished" || p.Body != "moved 3, failed 0" || p.Type != "success" {
			t.Fatalf("unexpected payload: %+v", p)
		}
	default:
		t.Fatal("Summary() did not send a notification")
	}
}

func TestNotifier_Item_OnlySendsWhenVerbose(t *testing.T) {
	srv, received := captureServer(t)

	n := &Notifier{URL: srv.URL, Verbose: false}
	n.Item("Moved Movie X", "to cold1", LevelInfo)
	select {
	case p := <-received:
		t.Fatalf("Item() sent while Verbose=false: %+v", p)
	default:
	}

	n.Verbose = true
	n.Item("Moved Movie X", "to cold1", LevelInfo)
	select {
	case p := <-received:
		if p.Title != "Moved Movie X" {
			t.Fatalf("unexpected payload: %+v", p)
		}
	default:
		t.Fatal("Item() did not send while Verbose=true")
	}
}

func TestNotifier_NilAndEmptyURLAreNoOps(t *testing.T) {
	var n *Notifier
	n.Summary("title", "body", LevelInfo) // must not panic
	n.Item("title", "body", LevelInfo)    // must not panic

	empty := &Notifier{Verbose: true}
	empty.Summary("title", "body", LevelInfo) // no URL - no dial, no panic
	empty.Item("title", "body", LevelInfo)
}

func TestNotifier_SendSwallowsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := &Notifier{URL: srv.URL}
	n.Summary("title", "body", LevelInfo) // must not panic or return anything to check
}

func TestTest_ReturnsErrorOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := Test(srv.URL, "", false); err == nil {
		t.Fatal("Test() returned nil error for a 500 response, want an error")
	}
}

func TestTest_Success(t *testing.T) {
	srv, received := captureServer(t)

	if err := Test(srv.URL, "", false); err != nil {
		t.Fatalf("Test() = %v, want nil", err)
	}
	select {
	case p := <-received:
		if p.Type != "info" {
			t.Fatalf("unexpected payload: %+v", p)
		}
		if p.Tag != "" {
			t.Fatalf("payload.Tag = %q, want empty when no tag configured", p.Tag)
		}
		if p.Format != "" {
			t.Fatalf("payload.Format = %q, want empty when markdown=false", p.Format)
		}
	default:
		t.Fatal("Test() did not send a notification")
	}
}

func TestTest_Markdown(t *testing.T) {
	srv, received := captureServer(t)

	if err := Test(srv.URL, "", true); err != nil {
		t.Fatalf("Test() = %v, want nil", err)
	}
	select {
	case p := <-received:
		if p.Format != "markdown" {
			t.Fatalf("payload.Format = %q, want %q", p.Format, "markdown")
		}
	default:
		t.Fatal("Test() did not send a notification")
	}
}

func TestNotifier_Summary_IncludesTagWhenSet(t *testing.T) {
	srv, received := captureServer(t)

	n := &Notifier{URL: srv.URL, Tag: "mobile"}
	n.Summary("title", "body", LevelInfo)

	select {
	case p := <-received:
		if p.Tag != "mobile" {
			t.Fatalf("payload.Tag = %q, want %q", p.Tag, "mobile")
		}
	default:
		t.Fatal("Summary() did not send a notification")
	}
}

func TestTest_IncludesTagWhenSet(t *testing.T) {
	srv, received := captureServer(t)

	if err := Test(srv.URL, "mobile", false); err != nil {
		t.Fatalf("Test() = %v, want nil", err)
	}
	select {
	case p := <-received:
		if p.Tag != "mobile" {
			t.Fatalf("payload.Tag = %q, want %q", p.Tag, "mobile")
		}
	default:
		t.Fatal("Test() did not send a notification")
	}
}

func TestNotifier_BoldCodeJoinLines_PassThroughWhenMarkdownOff(t *testing.T) {
	n := &Notifier{}
	if got := n.Bold("temphdd_path2"); got != "temphdd_path2" {
		t.Fatalf("Bold() = %q, want unchanged", got)
	}
	if got := n.Code("/data/tv"); got != "/data/tv" {
		t.Fatalf("Code() = %q, want unchanged", got)
	}
	if got := n.JoinLines([]string{"a", "b"}); got != "a; b" {
		t.Fatalf("JoinLines() = %q, want %q", got, "a; b")
	}
}

func TestNotifier_Bold_EscapesEmphasisCharsWhenMarkdownOn(t *testing.T) {
	n := &Notifier{Markdown: true}
	if got, want := n.Bold("temphdd_path2"), "**temphdd\\_path2**"; got != want {
		t.Fatalf("Bold() = %q, want %q", got, want)
	}
}

func TestNotifier_Code_DoesNotEscapeWhenMarkdownOn(t *testing.T) {
	n := &Notifier{Markdown: true}
	if got, want := n.Code("/data/tv_shows"), "`/data/tv_shows`"; got != want {
		t.Fatalf("Code() = %q, want %q", got, want)
	}
}

func TestNotifier_JoinLines_UsesNewlineWhenMarkdownOn(t *testing.T) {
	n := &Notifier{Markdown: true}
	if got, want := n.JoinLines([]string{"a", "b"}), "a\nb"; got != want {
		t.Fatalf("JoinLines() = %q, want %q", got, want)
	}
}

func TestNotifier_Summary_IncludesFormatWhenMarkdown(t *testing.T) {
	srv, received := captureServer(t)

	n := &Notifier{URL: srv.URL, Markdown: true}
	n.Summary("title", "**body**", LevelInfo)

	select {
	case p := <-received:
		if p.Format != "markdown" {
			t.Fatalf("payload.Format = %q, want %q", p.Format, "markdown")
		}
	default:
		t.Fatal("Summary() did not send a notification")
	}
}
