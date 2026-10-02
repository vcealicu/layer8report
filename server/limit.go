package main

import (
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// Limits are daily caps that reset at midnight UTC, plus a minimum gap between
// filings from one key. Without a proof of personhood the best proxy for
// "one human" is "one key, from a small number of networks".
type Limits struct {
	PerIP         int
	PerNet        int
	PerKey        int
	KeyGap        time.Duration
	NewKeysPerIP  int
	NewKeysPerNet int
}

var defaultLimits = Limits{
	PerIP:         24,
	PerNet:        60,
	PerKey:        12,
	KeyGap:        20 * time.Second,
	NewKeysPerIP:  3,
	NewKeysPerNet: 10,
}

// limiter state lives in memory only. IP addresses are never written to disk.
type limiter struct {
	mu      sync.Mutex
	lim     Limits
	day     string
	counts  map[string]int
	lastKey map[string]time.Time
	nonces  map[string]time.Time
	swept   time.Time
}

func newLimiter(l Limits) *limiter {
	return &limiter{
		lim:     l,
		counts:  map[string]int{},
		lastKey: map[string]time.Time{},
		nonces:  map[string]time.Time{},
	}
}

// netOf groups addresses the way ISPs hand them out: a /24 for IPv4 and a /48
// for IPv6, so rotating through one allocation does not reset the counters.
func netOf(ip netip.Addr) string {
	if ip.Is4() {
		p, _ := ip.Prefix(24)
		return p.String()
	}
	p, _ := ip.Prefix(48)
	return p.String()
}

// hostOf is the "one address" unit. IPv6 users usually get a whole /64,
// so a single IPv6 address would be far too fine grained.
func hostOf(ip netip.Addr) string {
	if ip.Is4() {
		return ip.String()
	}
	p, _ := ip.Prefix(64)
	return p.String()
}

type limitHit struct {
	Reason     string
	RetryAfter time.Duration
}

func (l *limiter) roll(now time.Time) {
	day := now.UTC().Format("2006-01-02")
	if day != l.day {
		l.day = day
		l.counts = map[string]int{}
	}
	if now.Sub(l.swept) > time.Minute {
		for k, exp := range l.nonces {
			if now.After(exp) {
				delete(l.nonces, k)
			}
		}
		for k, t := range l.lastKey {
			if now.Sub(t) > l.lim.KeyGap {
				delete(l.lastKey, k)
			}
		}
		l.swept = now
	}
}

func untilMidnight(now time.Time) time.Duration {
	u := now.UTC()
	next := time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
	return next.Sub(u)
}

// seenNonce reports whether this key already used this nonce.
func (l *limiter) seenNonce(kid, nonce string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	exp, ok := l.nonces[kid+":"+nonce]
	return ok && now.Before(exp)
}

// check returns nil when a filing may go ahead. It does not count anything.
func (l *limiter) check(ip netip.Addr, kid string, newKey bool, now time.Time) *limitHit {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)

	if last, ok := l.lastKey[kid]; ok {
		if wait := l.lim.KeyGap - now.Sub(last); wait > 0 {
			return &limitHit{fmt.Sprintf("one filing per key every %s", l.lim.KeyGap), wait}
		}
	}
	day := untilMidnight(now)
	ipKey, netKey := "ip:"+hostOf(ip), "net:"+netOf(ip)
	switch {
	case l.counts["key:"+kid] >= l.lim.PerKey:
		return &limitHit{fmt.Sprintf("%d filings per key per day", l.lim.PerKey), day}
	case l.counts[ipKey] >= l.lim.PerIP:
		return &limitHit{fmt.Sprintf("%d filings per IP address per day", l.lim.PerIP), day}
	case l.counts[netKey] >= l.lim.PerNet:
		return &limitHit{fmt.Sprintf("%d filings per network per day", l.lim.PerNet), day}
	}
	if newKey {
		switch {
		case l.counts["new"+ipKey] >= l.lim.NewKeysPerIP:
			return &limitHit{fmt.Sprintf("%d new keys per IP address per day; keep one key per human", l.lim.NewKeysPerIP), day}
		case l.counts["new"+netKey] >= l.lim.NewKeysPerNet:
			return &limitHit{fmt.Sprintf("%d new keys per network per day; keep one key per human", l.lim.NewKeysPerNet), day}
		}
	}
	return nil
}

// commit counts a stored filing and burns its nonce.
func (l *limiter) commit(ip netip.Addr, kid, nonce string, ts int64, newKey bool, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	ipKey, netKey := "ip:"+hostOf(ip), "net:"+netOf(ip)
	l.counts["key:"+kid]++
	l.counts[ipKey]++
	l.counts[netKey]++
	if newKey {
		l.counts["new"+ipKey]++
		l.counts["new"+netKey]++
	}
	l.lastKey[kid] = now
	l.nonces[kid+":"+nonce] = nonceExpiry(ts)
}

// A nonce only needs remembering while its timestamp could still pass the
// clock skew check. ts is whole seconds, so round up.
func nonceExpiry(ts int64) time.Time { return time.Unix(ts+ClockSkew+1, 0) }

// seed rebuilds what can be rebuilt after a restart: nonces still inside the
// replay window, today's per-key counts and the last filing time per key.
// Per-IP counts are never stored, so they start again from zero.
func (l *limiter) seed(recs []*Record, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	today := now.UTC().Format("2006-01-02")
	for _, r := range recs {
		if exp := nonceExpiry(r.Filing.TS); now.Before(exp) {
			l.nonces[r.KeyID+":"+r.Filing.Nonce] = exp
		}
		if r.At.UTC().Format("2006-01-02") == today {
			l.counts["key:"+r.KeyID]++
			if r.At.After(l.lastKey[r.KeyID]) {
				l.lastKey[r.KeyID] = r.At
			}
		}
	}
}
