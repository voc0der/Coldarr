package webui

import (
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// Password sign-ins are limited per client: signInAttempts in any
// signInWindow. An attempt counts from the moment it arrives until it turns
// out to be right, so guesses sent all at once count too.
const (
	signInWindow   = 15 * time.Minute
	signInAttempts = 10
	// signInClients bounds how many clients the limit keeps count for. Past
	// it, a client it isn't counting yet waits too: letting it through
	// uncounted would let enough addresses guess without limit.
	signInClients = 10000
)

// signInLimiter counts recent password sign-in attempts per client. Its zero
// value is ready to use.
type signInLimiter struct {
	mu      sync.Mutex
	clients map[netip.Prefix]*signInRecord
	// full is set once a refusal for want of room has been reported, until
	// there is room again.
	full bool
}

type signInRecord struct {
	attempts []time.Time // within the window, oldest first
	// refused is set once a refusal has been reported, until an attempt is
	// let through again.
	refused bool
}

// signInClient is who a request's attempts count against: the client's IPv4
// address, or the /64 its IPv6 address is in, since one IPv6 client usually
// has a whole /64 to pick addresses from. Behind a trusted proxy, the remote
// address is already the client's (see applyForwardedHeaders). Requests with
// no IP address all count against the zero prefix.
func signInClient(r *http.Request) netip.Prefix {
	addr, ok := remoteIP(r.RemoteAddr)
	if !ok {
		return netip.Prefix{}
	}
	bits := 32
	if addr.Is6() {
		bits = 64
	}
	prefix, _ := addr.WithZone("").Prefix(bits)
	return prefix
}

// clientAddr names a request's client address for the log.
func clientAddr(r *http.Request) string {
	if addr, ok := remoteIP(r.RemoteAddr); ok {
		return addr.WithZone("").String()
	}
	return "an unknown address"
}

// allow counts an attempt against client, at now, and reports whether it may
// go ahead. A refused attempt isn't counted. report is set on the first
// refusal since an attempt was last let through, so a client that keeps
// trying is logged once rather than on every try.
func (l *signInLimiter) allow(client netip.Prefix, now time.Time) (ok, report bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	since := now.Add(-signInWindow)
	rec := l.clients[client]
	if rec == nil {
		if len(l.clients) >= signInClients {
			l.prune(since)
		}
		if len(l.clients) >= signInClients {
			report, l.full = !l.full, true
			return false, report
		}
		if l.clients == nil {
			l.clients = map[netip.Prefix]*signInRecord{}
		}
		rec = &signInRecord{}
		l.clients[client] = rec
		l.full = false
	}
	rec.attempts = attemptsSince(rec.attempts, since)
	if len(rec.attempts) >= signInAttempts {
		report, rec.refused = !rec.refused, true
		return false, report
	}
	rec.attempts, rec.refused = append(rec.attempts, now), false
	return true, false
}

// forget drops the attempts counted against client, which has just signed
// in: it knows the password, so its earlier mistakes no longer matter.
func (l *signInLimiter) forget(client netip.Prefix) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clients, client)
}

// prune drops the clients with no attempt since since.
func (l *signInLimiter) prune(since time.Time) {
	for client, rec := range l.clients {
		if len(attemptsSince(rec.attempts, since)) == 0 {
			delete(l.clients, client)
		}
	}
}

// attemptsSince is the attempts at or after since, from a list oldest first.
func attemptsSince(attempts []time.Time, since time.Time) []time.Time {
	for i, at := range attempts {
		if !at.Before(since) {
			return attempts[i:]
		}
	}
	return nil
}
