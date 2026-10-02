package main

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// openLimits lets a test flood the server without tripping the per-caller
// limits, so the memory bounds are what get exercised.
var openLimits = Limits{
	KeyGap: 0, PerKeyDay: 1 << 30, PerIPHour: 1 << 30, PerIPDay: 1 << 30, PerNetHour: 1 << 30, PerNetDay: 1 << 30,
	NewKeysPerIPDay: 1 << 30, NewKeysPerNetDay: 1 << 30, GlobalHour: 1 << 30, GlobalDay: 1 << 30, DryRunsPerIPHour: 1 << 30,
	Strikes: 1 << 30, StrikeWindow: time.Hour, Block: time.Minute, BlockMax: time.Hour, MaxTracked: 10000,
}

func heapMB() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / 1e6
}

// TestMemoryStaysBounded files far more than the ring holds, from many keys
// and addresses, and checks that what stays in memory is the configured
// amount and nothing more.
func TestMemoryStaysBounded(t *testing.T) {
	const keep, humansCap, filings = 200, 300, 3000
	h := newHarnessWith(t, openLimits, keep, humansCap)
	before := heapMB()

	agents := make([]*agent, 0, 1000)
	for i := 0; i < 1000; i++ {
		agents = append(agents, newAgent())
	}
	long := strings.TrimSpace(strings.Repeat("Make it pop. ", 10)) // 129 chars, near the cap
	for i := 0; i < filings; i++ {
		a := agents[i%len(agents)]
		h.now = h.now.Add(time.Second)
		body := a.body(h, func(m map[string]any) {
			m["headline"] = long
			m["root_cause"] = long
			m["action_item"] = long
		})
		ip := fmt.Sprintf("%d.%d.%d.%d", 10+i%200, (i/7)%250, (i/3)%250, i%250)
		want(t, h.post(a, body, ip, ""), 201)
	}

	sz := h.s.st.sizes()
	if sz.Filings != filings || sz.Kept != keep || sz.Archived != filings-keep {
		t.Fatalf("store sizes: %+v", sz)
	}
	if sz.Humans > humansCap {
		t.Fatalf("humans tracked %d, cap %d", sz.Humans, humansCap)
	}
	ls := h.s.lim.sizes()
	for name, n := range map[string]int{"daily": ls.Daily, "hourly": ls.Hourly, "nonces": ls.Nonces, "strikes": ls.Strikes, "blocked": ls.Blocked} {
		if n > ls.Cap {
			t.Fatalf("limiter table %s has %d entries, cap %d", name, n, ls.Cap)
		}
	}
	after := heapMB()
	if grew := after - before; grew > 8 {
		t.Fatalf("heap grew by %.1f MB for %d filings; the bound is not holding", grew, filings)
	}
	t.Logf("heap grew %.1f MB for %d filings (%d kept in full, %d humans tracked)", after-before, filings, sz.Kept, sz.Humans)

	// The totals still count everything, and old filings are archived, not lost.
	st := want(t, h.get("/api/v1/stats"), 200)
	if st["totals"].(map[string]any)["filings"].(float64) != filings {
		t.Fatalf("totals: %v", st["totals"])
	}
	if m := want(t, h.get("/api/v1/reports/1"), 410); m["error"] != "archived" {
		t.Fatalf("old record: %v", m)
	}
	want(t, h.get("/api/v1/reports/"+idFor(filings)), 200)
	feed := want(t, h.get("/api/v1/reports?limit=5"), 200)
	if feed["archived"].(float64) != filings-keep || feed["kept_in_full"].(float64) != keep {
		t.Fatalf("feed: archived=%v kept=%v", feed["archived"], feed["kept_in_full"])
	}
	if md := h.get("/api/v1/reports.md").Body.String(); !strings.Contains(md, "live on in the totals") {
		t.Fatalf("feed md lacks the archive note")
	}
}

func TestLimiterTablesAreCapped(t *testing.T) {
	lim := openLimits
	lim.MaxTracked = 50
	h := newHarnessWith(t, lim, 50, 50)
	for i := 0; i < 400; i++ {
		a := newAgent()
		h.now = h.now.Add(time.Second)
		want(t, h.post(a, a.body(h, nil), fmt.Sprintf("192.0.%d.%d", i/250, i%250), ""), 201)
	}
	ls := h.s.lim.sizes()
	if ls.Daily > 50 || ls.Hourly > 50 || ls.Nonces > 50 {
		t.Fatalf("tables over cap: %+v", ls)
	}
	if n := h.s.st.sizes().Humans; n > 50 {
		t.Fatalf("humans over cap: %d", n)
	}
}

func TestPenaltyBox(t *testing.T) {
	lim := defaultLimits
	lim.Strikes, lim.Block, lim.BlockMax = 5, 10*time.Minute, time.Hour
	h := newHarnessWith(t, lim, 500, 5000)
	a := newAgent()
	ip := "203.0.113.50"

	// Unsigned attempts strike the address.
	for i := 0; i < 4; i++ {
		want(t, h.post(nil, a.body(h, nil), ip, ""), 401)
	}
	want(t, h.post(nil, a.body(h, nil), ip, ""), 401) // fifth strike places the block
	rr := h.post(a, a.body(h, nil), ip, "")
	m := want(t, rr, 429)
	if m["error"] != "cooling_off" || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("expected penalty box, got %v", m)
	}
	// Dry runs are blocked too; the point is to stop the traffic.
	if m := want(t, h.post(a, a.body(h, nil), ip, "?dry_run=1"), 429); m["error"] != "cooling_off" {
		t.Fatalf("dry run during block: %v", m)
	}
	// Another address is unaffected.
	want(t, h.post(a, a.body(h, nil), "203.0.113.51", ""), 201)

	// The block expires, then a second offence doubles it.
	h.now = h.now.Add(11 * time.Minute)
	want(t, h.post(a, a.body(h, nil), ip, ""), 201)
	for i := 0; i < 5; i++ {
		h.post(nil, a.body(h, nil), ip, "")
	}
	rr = h.post(a, a.body(h, nil), ip, "")
	want(t, rr, 429)
	var secs int
	fmt.Sscanf(rr.Header().Get("Retry-After"), "%d", &secs)
	if secs < 19*60 || secs > 21*60 {
		t.Fatalf("second block should be about 20 minutes, got %ds", secs)
	}
}

func TestKeyBlockFollowsRotatingAddresses(t *testing.T) {
	lim := defaultLimits
	lim.Strikes = 5
	h := newHarnessWith(t, lim, 500, 5000)
	a := newAgent()
	bad := a.body(h, func(m map[string]any) { m["tags"] = []string{"nope"} })
	for i := 0; i < 5; i++ {
		want(t, h.post(a, bad, fmt.Sprintf("198.51.100.%d", i+1), ""), 422)
	}
	// Same key, fresh address: still blocked, because the key itself is.
	if m := want(t, h.post(a, a.body(h, nil), "198.51.100.200", ""), 429); m["error"] != "cooling_off" {
		t.Fatalf("key block: %v", m)
	}
}

func TestGlobalPauseIsNobodysFault(t *testing.T) {
	lim := openLimits
	lim.GlobalHour, lim.Strikes = 3, 2
	h := newHarnessWith(t, lim, 500, 5000)
	for i := 0; i < 3; i++ {
		a := newAgent()
		want(t, h.post(a, a.body(h, nil), fmt.Sprintf("192.0.2.%d", i+1), ""), 201)
	}
	a := newAgent()
	m := want(t, h.post(a, a.body(h, nil), "192.0.2.9", ""), 429)
	if m["error"] != "paused" || !strings.Contains(m["message"].(string), "paused for everyone") {
		t.Fatalf("pause: %v", m)
	}
	// Hitting the pause repeatedly is not a strike.
	for i := 0; i < 5; i++ {
		want(t, h.post(a, a.body(h, nil), "192.0.2.9", ""), 429)
	}
	if h.s.lim.sizes().Blocked != 0 {
		t.Fatal("a paused caller was put in the penalty box")
	}
	// Next hour, filing resumes.
	h.now = h.now.Truncate(time.Hour).Add(time.Hour)
	want(t, h.post(a, a.body(h, nil), "192.0.2.9", ""), 201)
}

func TestDryRunsAreBudgeted(t *testing.T) {
	lim := defaultLimits
	lim.DryRunsPerIPHour = 2
	h := newHarnessWith(t, lim, 500, 5000)
	a := newAgent()
	want(t, h.post(a, a.body(h, nil), "203.0.113.70", "?dry_run=1"), 200)
	want(t, h.post(a, a.body(h, nil), "203.0.113.70", "?dry_run=1"), 200)
	if m := want(t, h.post(a, a.body(h, nil), "203.0.113.70", "?dry_run=1"), 429); !strings.Contains(m["message"].(string), "dry runs") {
		t.Fatalf("dry run budget: %v", m)
	}
	// A real filing is not a dry run and still goes through.
	want(t, h.post(a, a.body(h, nil), "203.0.113.70", ""), 201)
}

func TestBodySizeCap(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	big := a.body(h, func(m map[string]any) { m["headline"] = strings.Repeat("x", MaxBody) })
	if m := want(t, h.post(a, big, "203.0.113.80", ""), 413); m["error"] != "body_too_large" {
		t.Fatalf("%v", m)
	}
	// Three full-length fields in a four-byte script still fit.
	cjk := strings.Repeat("\U0001F600", MaxHeadline)
	ok := a.body(h, func(m map[string]any) { m["headline"], m["root_cause"], m["action_item"] = cjk, cjk, cjk })
	if len(ok) > MaxBody {
		t.Fatalf("worst-case body is %d bytes, over MaxBody %d", len(ok), MaxBody)
	}
	want(t, h.post(a, ok, "203.0.113.80", "?dry_run=1"), 200)
}

func TestHumansEvictOldestAndCardStillWorks(t *testing.T) {
	h := newHarnessWith(t, openLimits, 50, 10)
	first := newAgent()
	want(t, h.post(first, first.body(h, nil), "192.0.2.1", ""), 201)
	for i := 0; i < 12; i++ {
		a := newAgent()
		h.now = h.now.Add(time.Minute)
		want(t, h.post(a, a.body(h, nil), fmt.Sprintf("192.0.2.%d", i+2), ""), 201)
	}
	// The first human was the least recently seen and has been dropped.
	want(t, h.get("/api/v1/humans/"+keyID(first.pub)), 404)
	if n := h.s.st.sizes().Humans; n > 10 {
		t.Fatalf("humans %d over cap", n)
	}
	// They come back as a new key if they file again, and the card works.
	h.now = h.now.Add(time.Minute)
	want(t, h.post(first, first.body(h, nil), "192.0.2.1", ""), 201)
	card := want(t, h.get("/api/v1/humans/"+keyID(first.pub)), 200)
	if card["human"].(map[string]any)["filings"].(float64) != 1 {
		t.Fatalf("card after eviction: %v", card["human"])
	}
}

func TestHealthReportsMemory(t *testing.T) {
	h := newHarness(t)
	m := want(t, h.get("/api/v1/health"), 200)
	mem := m["memory"].(map[string]any)
	if mem["heap_mb"] == nil || m["store"] == nil || m["limiter"] == nil {
		t.Fatalf("health: %v", m)
	}
}
