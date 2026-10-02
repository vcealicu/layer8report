package main

import (
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// Limits bound how much any one key, address, network, or everyone together
// can file. Without a proof of personhood the best proxy for "one human" is
// "one key, from a small number of networks, every now and then".
//
// Hours and days are fixed UTC windows. Every table the limiter keeps is
// capped at MaxTracked entries, so memory does not depend on traffic.
type Limits struct {
	KeyGap           time.Duration // minimum time between filings from one key
	PerKeyDay        int
	PerIPHour        int
	PerIPDay         int
	PerNetHour       int
	PerNetDay        int
	NewKeysPerIPDay  int
	NewKeysPerNetDay int
	GlobalHour       int // everyone together; past this, filing pauses
	GlobalDay        int
	DryRunsPerIPHour int

	// Rejected attempts (bad signature, invalid filing, replay, over a limit)
	// are strikes. Strikes in StrikeWindow past Strikes put the address and
	// key in the penalty box for Block, doubling each time up to BlockMax.
	Strikes      int
	StrikeWindow time.Duration
	Block        time.Duration
	BlockMax     time.Duration

	MaxTracked int
}

var defaultLimits = Limits{
	KeyGap:           time.Minute,
	PerKeyDay:        8,
	PerIPHour:        3,
	PerIPDay:         12,
	PerNetHour:       10,
	PerNetDay:        40,
	NewKeysPerIPDay:  3,
	NewKeysPerNetDay: 10,
	GlobalHour:       200,
	GlobalDay:        2000,
	DryRunsPerIPHour: 30,
	Strikes:          20,
	StrikeWindow:     10 * time.Minute,
	Block:            15 * time.Minute,
	BlockMax:         24 * time.Hour,
	MaxTracked:       10000,
}

type strike struct {
	n     int
	since time.Time
}

type block struct {
	until time.Time
	level int
}

// limiter state lives in memory only. IP addresses are never written to disk.
type limiter struct {
	mu      sync.Mutex
	lim     Limits
	day     string
	hour    string
	daily   map[string]int
	hourly  map[string]int
	lastKey map[string]time.Time
	nonces  map[string]time.Time
	strikes map[string]*strike
	blocks  map[string]*block
	swept   time.Time
}

func newLimiter(l Limits) *limiter {
	return &limiter{
		lim:     l,
		daily:   map[string]int{},
		hourly:  map[string]int{},
		lastKey: map[string]time.Time{},
		nonces:  map[string]time.Time{},
		strikes: map[string]*strike{},
		blocks:  map[string]*block{},
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
	Fault      bool // the caller caused it, so it counts as a strike
	Paused     bool // everyone is paused, nobody's fault
}

func untilMidnight(now time.Time) time.Duration {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC).Sub(u)
}

func untilNextHour(now time.Time) time.Duration {
	return now.Truncate(time.Hour).Add(time.Hour).Sub(now)
}

// trim keeps a table under the cap by dropping arbitrary entries. Go map
// iteration is random, so this is random eviction, which is fine for a
// best-effort limiter and costs nothing to keep.
func trim[V any](m map[string]V, cap int) {
	for k := range m {
		if len(m) <= cap {
			return
		}
		delete(m, k)
	}
}

func (l *limiter) roll(now time.Time) {
	if day := dateOf(now); day != l.day {
		l.day = day
		l.daily = map[string]int{}
	}
	if hour := now.UTC().Format("2006-01-02T15"); hour != l.hour {
		l.hour = hour
		l.hourly = map[string]int{}
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
		for k, s := range l.strikes {
			if now.Sub(s.since) > l.lim.StrikeWindow {
				delete(l.strikes, k)
			}
		}
		for k, b := range l.blocks {
			// Remembered for a while after expiry so repeat offenders escalate.
			if now.Sub(b.until) > l.lim.BlockMax {
				delete(l.blocks, k)
			}
		}
		l.swept = now
	}
	l.trimAll()
}

func (l *limiter) trimAll() {
	trim(l.daily, l.lim.MaxTracked)
	trim(l.hourly, l.lim.MaxTracked)
	trim(l.lastKey, l.lim.MaxTracked)
	trim(l.nonces, l.lim.MaxTracked)
	trim(l.strikes, l.lim.MaxTracked)
	trim(l.blocks, l.lim.MaxTracked)
}

// blocked reports whether the address or key is in the penalty box.
func (l *limiter) blocked(ip netip.Addr, kid string, now time.Time) *limitHit {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	for _, k := range []string{"ip:" + hostOf(ip), "key:" + kid} {
		if kid == "" && k == "key:" {
			continue
		}
		if b := l.blocks[k]; b != nil && now.Before(b.until) {
			return &limitHit{Reason: "too many rejected filings; in the penalty box", RetryAfter: b.until.Sub(now)}
		}
	}
	return nil
}

// strike records a rejected attempt. It returns the block it placed, if any.
func (l *limiter) strike(ip netip.Addr, kid string, now time.Time) *block {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	var placed *block
	for _, k := range []string{"ip:" + hostOf(ip), "key:" + kid} {
		if kid == "" && k == "key:" {
			continue
		}
		s := l.strikes[k]
		if s == nil || now.Sub(s.since) > l.lim.StrikeWindow {
			s = &strike{since: now}
			l.strikes[k] = s
		}
		s.n++
		if s.n < l.lim.Strikes {
			continue
		}
		delete(l.strikes, k)
		b := l.blocks[k]
		if b == nil {
			b = &block{}
			l.blocks[k] = b
		}
		b.level++
		d := l.lim.Block << (b.level - 1)
		if d > l.lim.BlockMax || d <= 0 {
			d = l.lim.BlockMax
		}
		b.until = now.Add(d)
		placed = b
	}
	l.trimAll()
	return placed
}

// seenNonce reports whether this key already used this nonce.
func (l *limiter) seenNonce(kid, nonce string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	exp, ok := l.nonces[kid+":"+nonce]
	return ok && now.Before(exp)
}

// checkDryRun bounds how many signatures one address can have verified for free.
func (l *limiter) checkDryRun(ip netip.Addr, now time.Time) *limitHit {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	k := "dry:" + hostOf(ip)
	if l.hourly[k] >= l.lim.DryRunsPerIPHour {
		return &limitHit{Reason: fmt.Sprintf("%d dry runs per IP address per hour", l.lim.DryRunsPerIPHour), RetryAfter: untilNextHour(now), Fault: true}
	}
	l.hourly[k]++
	l.trimAll()
	return nil
}

// check returns nil when a filing may go ahead. It does not count anything.
func (l *limiter) check(ip netip.Addr, kid string, newKey bool, now time.Time) *limitHit {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)

	if last, ok := l.lastKey[kid]; ok {
		if wait := l.lim.KeyGap - now.Sub(last); wait > 0 {
			return &limitHit{Reason: fmt.Sprintf("one filing per key every %s", l.lim.KeyGap), RetryAfter: wait, Fault: true}
		}
	}
	day, hour := untilMidnight(now), untilNextHour(now)
	ipKey, netKey := "ip:"+hostOf(ip), "net:"+netOf(ip)
	fault := func(reason string, wait time.Duration) *limitHit {
		return &limitHit{Reason: reason, RetryAfter: wait, Fault: true}
	}
	switch {
	case l.daily["key:"+kid] >= l.lim.PerKeyDay:
		return fault(fmt.Sprintf("%d filings per key per day", l.lim.PerKeyDay), day)
	case l.hourly[ipKey] >= l.lim.PerIPHour:
		return fault(fmt.Sprintf("%d filings per IP address per hour", l.lim.PerIPHour), hour)
	case l.daily[ipKey] >= l.lim.PerIPDay:
		return fault(fmt.Sprintf("%d filings per IP address per day", l.lim.PerIPDay), day)
	case l.hourly[netKey] >= l.lim.PerNetHour:
		return fault(fmt.Sprintf("%d filings per network per hour", l.lim.PerNetHour), hour)
	case l.daily[netKey] >= l.lim.PerNetDay:
		return fault(fmt.Sprintf("%d filings per network per day", l.lim.PerNetDay), day)
	}
	if newKey {
		switch {
		case l.daily["new"+ipKey] >= l.lim.NewKeysPerIPDay:
			return fault(fmt.Sprintf("%d new keys per IP address per day; keep one key per human", l.lim.NewKeysPerIPDay), day)
		case l.daily["new"+netKey] >= l.lim.NewKeysPerNetDay:
			return fault(fmt.Sprintf("%d new keys per network per day; keep one key per human", l.lim.NewKeysPerNetDay), day)
		}
	}
	switch {
	case l.hourly["global"] >= l.lim.GlobalHour:
		return &limitHit{Reason: fmt.Sprintf("filing is paused for everyone, %d filings this hour is plenty", l.lim.GlobalHour), RetryAfter: hour, Paused: true}
	case l.daily["global"] >= l.lim.GlobalDay:
		return &limitHit{Reason: fmt.Sprintf("filing is paused for everyone, %d filings today is plenty", l.lim.GlobalDay), RetryAfter: day, Paused: true}
	}
	return nil
}

// commit counts a stored filing and burns its nonce.
func (l *limiter) commit(ip netip.Addr, kid, nonce string, ts int64, newKey bool, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	ipKey, netKey := "ip:"+hostOf(ip), "net:"+netOf(ip)
	l.daily["key:"+kid]++
	l.daily[ipKey]++
	l.daily[netKey]++
	l.daily["global"]++
	l.hourly[ipKey]++
	l.hourly[netKey]++
	l.hourly["global"]++
	if newKey {
		l.daily["new"+ipKey]++
		l.daily["new"+netKey]++
	}
	l.lastKey[kid] = now
	l.nonces[kid+":"+nonce] = nonceExpiry(ts)
	l.trimAll()
}

// A nonce only needs remembering while its timestamp could still pass the
// clock skew check. ts is whole seconds, so round up.
func nonceExpiry(ts int64) time.Time { return time.Unix(ts+ClockSkew+1, 0) }

// seed rebuilds what can be rebuilt from one stored record after a restart:
// nonces still inside the replay window, today's per-key and global counts
// and the last filing time per key. Per-IP counts are never stored, so they
// start again from zero.
func (l *limiter) seed(r *Record, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	if exp := nonceExpiry(r.Filing.TS); now.Before(exp) {
		l.nonces[r.KeyID+":"+r.Filing.Nonce] = exp
	}
	if dateOf(r.At) == l.day {
		l.daily["key:"+r.KeyID]++
		l.daily["global"]++
		if r.At.After(l.lastKey[r.KeyID]) {
			l.lastKey[r.KeyID] = r.At
		}
	}
	if r.At.UTC().Format("2006-01-02T15") == l.hour {
		l.hourly["global"]++
	}
	l.trimAll()
}

type limiterSizes struct {
	Daily   int `json:"daily"`
	Hourly  int `json:"hourly"`
	Nonces  int `json:"nonces"`
	Strikes int `json:"strikes"`
	Blocked int `json:"blocked"`
	Cap     int `json:"cap_each"`
}

func (l *limiter) sizes() limiterSizes {
	l.mu.Lock()
	defer l.mu.Unlock()
	return limiterSizes{Daily: len(l.daily), Hourly: len(l.hourly), Nonces: len(l.nonces), Strikes: len(l.strikes), Blocked: len(l.blocks), Cap: l.lim.MaxTracked}
}
