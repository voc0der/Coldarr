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

	if err := Test(srv.URL); err == nil {
		t.Fatal("Test() returned nil error for a 500 response, want an error")
	}
}

func TestTest_Success(t *testing.T) {
	srv, received := captureServer(t)

	if err := Test(srv.URL); err != nil {
		t.Fatalf("Test() = %v, want nil", err)
	}
	select {
	case p := <-received:
		if p.Type != "info" {
			t.Fatalf("unexpected payload: %+v", p)
		}
	default:
		t.Fatal("Test() did not send a notification")
	}
}
