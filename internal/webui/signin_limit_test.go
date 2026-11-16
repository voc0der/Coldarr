package webui

import (
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// logBuffer collects log output; safe for goroutines a test leaves running.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureLog sends the standard logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *logBuffer {
	t.Helper()
	buf := &logBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

func TestSignInLimiter_RefusesPastTheLimitUntilAttemptsAge(t *testing.T) {
	var l signInLimiter
	client := netip.MustParsePrefix("203.0.113.7/32")
	start := time.Now()

	for i := range signInAttempts {
		if ok, _ := l.allow(client, start.Add(time.Duration(i)*time.Minute)); !ok {
			t.Fatalf("attempt %d refused, want %d allowed", i+1, signInAttempts)
		}
	}
	last := start.Add(time.Duration(signInAttempts-1) * time.Minute)
	ok, report := l.allow(client, last)
	if ok || !report {
		t.Fatalf("attempt past the limit = %v, report %v; want refused and reported", ok, report)
	}
	if ok, report := l.allow(client, last.Add(time.Second)); ok || report {
		t.Fatalf("a second refusal = %v, report %v; want refused without another report", ok, report)
	}

	// The window slides: once the first attempt is older than it, one more
	// goes through, and the next refusal is reported again.
	aged := start.Add(signInWindow + time.Second)
	if ok, _ := l.allow(client, aged); !ok {
		t.Fatal("an attempt after the oldest one left the window was refused")
	}
	if ok, report := l.allow(client, aged); ok || !report {
		t.Fatalf("back at the limit = %v, report %v; want refused and reported", ok, report)
	}
}

func TestSignInLimiter_ForgetClearsAClient(t *testing.T) {
	var l signInLimiter
	client := netip.MustParsePrefix("203.0.113.7/32")
	now := time.Now()
	for range signInAttempts {
		l.allow(client, now)
	}
	if ok, _ := l.allow(client, now); ok {
		t.Fatal("expected the client to be at its limit")
	}
	l.forget(client)
	if ok, _ := l.allow(client, now); !ok {
		t.Fatal("a forgotten client should start over")
	}
}

// TestSignInLimiter_FullRefusesNewClientsUntilRoomFrees: with signInClients
// clients counted, a new one waits rather than guessing uncounted - until
// clients with nothing recent can make room.
func TestSignInLimiter_FullRefusesNewClientsUntilRoomFrees(t *testing.T) {
	var l signInLimiter
	now := time.Now()
	for i := range signInClients {
		addr := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		if ok, _ := l.allow(netip.PrefixFrom(addr, 32), now); !ok {
			t.Fatalf("client %d refused before the limiter was full", i)
		}
	}
	newcomer := netip.MustParsePrefix("203.0.113.7/32")
	if ok, report := l.allow(newcomer, now); ok || !report {
		t.Fatalf("a new client with the limiter full = %v, report %v; want refused and reported", ok, report)
	}
	if ok, report := l.allow(newcomer, now); ok || report {
		t.Fatalf("again = %v, report %v; want refused without another report", ok, report)
	}
	// A client already counted still gets its own attempts.
	if ok, _ := l.allow(netip.MustParsePrefix("10.0.0.0/32"), now); !ok {
		t.Fatal("a client already counted was refused because the limiter is full")
	}

	later := now.Add(signInWindow + time.Second)
	if ok, _ := l.allow(newcomer, later); !ok {
		t.Fatal("a new client was refused after the others' attempts aged out")
	}
	if l.full || len(l.clients) != 1 {
		t.Fatalf("after pruning: full %v, %d clients; want room again and just the newcomer", l.full, len(l.clients))
	}
}

func TestSignInClient(t *testing.T) {
	tests := []struct {
		remote, want string
	}{
		{remote: "203.0.113.7:4711", want: "203.0.113.7/32"},
		{remote: "203.0.113.7", want: "203.0.113.7/32"},
		{remote: "[::ffff:203.0.113.7]:4711", want: "203.0.113.7/32"},
		// One IPv6 client usually has a whole /64.
		{remote: "[2001:db8:1:2:aaaa::1]:4711", want: "2001:db8:1:2::/64"},
		{remote: "[2001:db8:1:2:bbbb::9]:4711", want: "2001:db8:1:2::/64"},
		{remote: "2001:db8:1:3::1", want: "2001:db8:1:3::/64"},
		{remote: "[fe80::1%eth0]:4711", want: "fe80::/64"},
		{remote: "unix-socket", want: "invalid Prefix"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodPost, "/login", nil)
		r.RemoteAddr = tt.remote
		if got := signInClient(r).String(); got != tt.want {
			t.Errorf("signInClient(%q) = %s, want %s", tt.remote, got, tt.want)
		}
	}
}

// postPassword submits the password form from remote.
func postPassword(handler http.Handler, remote, password string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password="+password+"&return_to=/plan"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = remote
	handler.ServeHTTP(rec, req)
	return rec
}

// TestPasswordSignIn_LimitsFailedAttemptsPerClient: past signInAttempts
// wrong passwords, a client waits out the window - even with the right
// password, or guessing would just go on - while other clients sign in as
// usual. Every failure and the first refusal are logged.
func TestPasswordSignIn_LimitsFailedAttemptsPerClient(t *testing.T) {
	t.Setenv(passwordEnvVar, "pw")
	logged := captureLog(t)
	handler := newAuthTestServer(t, false).routes()

	for i := range signInAttempts {
		rec := postPassword(handler, "203.0.113.7:1000", "nope")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Incorrect password") {
			t.Fatalf("wrong password %d = %d, want the form again saying so", i+1, rec.Code)
		}
	}
	if got := strings.Count(logged.String(), "auth: wrong password from 203.0.113.7"); got != signInAttempts {
		t.Fatalf("logged %d wrong passwords, want %d:\n%s", got, signInAttempts, logged)
	}

	for range 2 {
		rec := postPassword(handler, "203.0.113.7:1001", "pw")
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "900" {
			t.Fatalf("right password past the limit = %d, Retry-After %q; want 429 and 900", rec.Code, rec.Header().Get("Retry-After"))
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Too many failed sign-in attempts") || !strings.Contains(body, `name="password"`) || !strings.Contains(body, `value="/plan"`) {
			t.Fatalf("refused sign-in page doesn't say why, or lost the form:\n%s", body)
		}
		if sessionCookie(rec) != nil {
			t.Fatal("a refused sign-in set a session cookie")
		}
	}
	if got := strings.Count(logged.String(), "auth: refusing password sign-ins from 203.0.113.7: too many failed attempts"); got != 1 {
		t.Fatalf("logged %d refusals, want 1:\n%s", got, logged)
	}

	if rec := postPassword(handler, "198.51.100.4:1000", "pw"); rec.Code != http.StatusFound || sessionCookie(rec) == nil {
		t.Fatalf("another client's sign-in = %d, want it signed in", rec.Code)
	}
	if !strings.Contains(logged.String(), "auth: signed in with the password from 198.51.100.4") {
		t.Fatalf("the sign-in wasn't logged:\n%s", logged)
	}
}

// TestPasswordSignIn_SuccessForgetsEarlierMistakes: a client that signs in
// knows the password, so its earlier wrong ones stop counting.
func TestPasswordSignIn_SuccessForgetsEarlierMistakes(t *testing.T) {
	t.Setenv(passwordEnvVar, "pw")
	handler := newAuthTestServer(t, false).routes()

	for range signInAttempts - 1 {
		postPassword(handler, "203.0.113.7:1000", "nope")
	}
	if rec := postPassword(handler, "203.0.113.7:1000", "pw"); rec.Code != http.StatusFound {
		t.Fatalf("right password = %d, want signed in", rec.Code)
	}
	for i := range signInAttempts {
		if rec := postPassword(handler, "203.0.113.7:1000", "nope"); rec.Code != http.StatusOK {
			t.Fatalf("wrong password %d after signing in = %d, want it counted from zero", i+1, rec.Code)
		}
	}
	if rec := postPassword(handler, "203.0.113.7:1000", "nope"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("past the limit again = %d, want 429", rec.Code)
	}
}
