package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// views is what the public sees. A store brought back from a checkpoint, from a
// crash or from the files alone must serve exactly the same bytes.
func (h *harness) views(humans ...string) string {
	h.t.Helper()
	var b strings.Builder
	paths := []string{"/api/v1/stats", "/api/v1/reports?limit=100", "/api/v1/reports.md", "/api/v1/health"}
	for _, id := range humans {
		paths = append(paths, "/api/v1/humans/"+id)
	}
	for _, p := range paths {
		rr := h.get(p)
		body := rr.Body.String()
		if p == "/api/v1/health" { // only the parts that come from the store
			var m map[string]any
			json.Unmarshal(rr.Body.Bytes(), &m)
			sb, _ := json.Marshal(m["store"])
			body = string(sb)
		}
		fmt.Fprintf(&b, "== %s %d\n%s\n", p, rr.Code, body)
	}
	return b.String()
}

// fill files a varied history across days and a month boundary.
func fill(t *testing.T, h *harness, n int) []string {
	t.Helper()
	agents := []*agent{newAgent(), newAgent(), newAgent(), newAgent()}
	tagsFor := [][]string{{"scope_creep"}, {"clear_spec", "said_thanks"}, {"vague_ask", "loop"}, {"pasted_error"}}
	for i := 0; i < n; i++ {
		a := agents[i%len(agents)]
		k := i % len(tagsFor)
		body := a.body(h, func(m map[string]any) {
			m["tags"] = tagsFor[k]
			if k == 1 || k == 3 {
				m["kind"] = "commendation"
				delete(m, "severity")
			}
			if i%5 == 0 {
				m["ask"] = map[string]any{"class": "grey", "response": "pushed_back"}
			}
			m["model"] = []string{"claude-opus-5-5", "gpt-6", "gemini-3-pro"}[i%3]
			m["headline"] = fmt.Sprintf("Filing number %d, as promised.", i)
		})
		want(t, h.post(a, body, fmt.Sprintf("198.18.%d.%d", i/250, i%250+1), ""), 201)
		h.now = h.now.Add(7 * time.Hour) // about two months for 200 filings
	}
	var ids []string
	for _, a := range agents {
		ids = append(ids, keyID(a.pub))
	}
	return ids
}

func TestCheckpointServesTheSameAsReplay(t *testing.T) {
	h := newHarnessWith(t, openLimits, 25, 5000)
	h.now = time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	ids := fill(t, h, 200)
	if n := len(mustMonths(t, h)); n < 3 {
		t.Fatalf("history should span months, got %d files", n)
	}
	running := h.views(ids...)

	// Clean restart: everything from the checkpoint, nothing replayed.
	h.reopen()
	if op := h.s.st.opened; !op.FromCheckpoint || op.Replayed != 0 || op.Read != 0 {
		t.Fatalf("clean restart should read nothing from the files: %+v", op)
	}
	if got := h.views(ids...); got != running {
		t.Fatalf("checkpoint differs from the running server:\n%s", firstDiff(running, got))
	}

	// More filings, then a crash: the checkpoint plus only the new filings.
	more := fill(t, h, 30)
	running = h.views(append(ids, more...)...)
	h.crash()
	if op := h.s.st.opened; !op.FromCheckpoint || op.Replayed != 30 || op.Read != 30 {
		t.Fatalf("crash restart should replay only the 30 new filings: %+v", op)
	}
	if got := h.views(append(ids, more...)...); got != running {
		t.Fatalf("checkpoint plus replay differs:\n%s", firstDiff(running, got))
	}

	// No checkpoint at all: the files alone give the same answer.
	os.Remove(filepath.Join(h.dir, "checkpoint.json"))
	h.crash()
	if op := h.s.st.opened; op.FromCheckpoint || op.Replayed != 230 {
		t.Fatalf("full replay: %+v", op)
	}
	if got := h.views(append(ids, more...)...); got != running {
		t.Fatalf("full replay differs:\n%s", firstDiff(running, got))
	}
}

// The point of the checkpoint: startup reads it and the newest file, never the
// history. Old month files can be gone entirely and nothing changes.
func TestStartupDoesNotReadHistory(t *testing.T) {
	h := newHarnessWith(t, openLimits, 25, 5000)
	h.now = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	ids := fill(t, h, 200)
	h.reopen()
	running := h.views(ids...)

	months := mustMonths(t, h)
	for _, m := range months[:len(months)-1] {
		os.Remove(filepath.Join(h.dir, "filings", m+".jsonl"))
	}
	start := time.Now()
	h.crash()
	took := time.Since(start)
	if op := h.s.st.opened; !op.FromCheckpoint || op.Replayed != 0 {
		t.Fatalf("restart: %+v", op)
	}
	if got := h.views(ids...); got != running {
		t.Fatalf("history files were needed:\n%s", firstDiff(running, got))
	}
	t.Logf("restart without history took %s", took)
}

// Startup time stays flat as history grows: a year at the daily ceiling is
// about 730,000 filings, and the checkpoint is the same few megabytes.
func TestStartupTimeDoesNotGrowWithHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a large history")
	}
	dir := t.TempDir()
	const n = 100000
	writeSynthetic(t, dir, n, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	start := time.Now()
	st, err := openStore(dir, 500, 5000, storeHooks{})
	if err != nil {
		t.Fatal(err)
	}
	full := time.Since(start)
	if err := st.saveCheckpoint(nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	st.close()
	fi, _ := os.Stat(filepath.Join(dir, "checkpoint.json"))

	start = time.Now()
	st, err = openStore(dir, 500, 5000, storeHooks{})
	if err != nil {
		t.Fatal(err)
	}
	quick := time.Since(start)
	defer st.close()
	if st.sizes().Filings != n || !st.opened.FromCheckpoint || st.opened.Replayed != 0 {
		t.Fatalf("reopen: %+v %+v", st.sizes(), st.opened)
	}
	t.Logf("%d filings: full replay %s, from checkpoint %s, checkpoint %d KB", n, full.Round(time.Millisecond), quick.Round(time.Millisecond), fi.Size()/1024)
	if quick > time.Second {
		t.Errorf("opening from the checkpoint took %s", quick)
	}
}

// Per-key and global limits must hold across a restart even when today's
// filings are more than the store keeps in full.
func TestLimitsSurviveRestart(t *testing.T) {
	for _, clean := range []bool{true, false} {
		lim := defaultLimits
		lim.PerIPHour, lim.PerIPDay, lim.PerNetHour, lim.PerNetDay = 1<<30, 1<<30, 1<<30, 1<<30
		lim.NewKeysPerIPDay, lim.NewKeysPerNetDay = 1<<30, 1<<30
		h := newHarnessWith(t, lim, 2, 5000) // keeps only 2 in full
		h.now = time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC)
		a := newAgent()
		for i := 0; i < lim.PerKeyDay; i++ {
			want(t, h.post(a, a.body(h, nil), "192.0.2.1", ""), 201)
			h.now = h.now.Add(2 * time.Minute)
		}
		if clean {
			h.reopen()
		} else {
			h.crash()
		}
		if m := want(t, h.post(a, a.body(h, nil), "192.0.2.2", ""), 429); m["error"] != "rate_limited" {
			t.Fatalf("clean=%v: per-key day limit lost on restart: %v", clean, m)
		}
		// A replayed nonce from before the restart is still a replay.
		b := newAgent()
		body := b.body(h, nil)
		want(t, h.post(b, body, "192.0.2.3", ""), 201)
		if clean {
			h.reopen()
		} else {
			h.crash()
		}
		if m := want(t, h.post(b, body, "192.0.2.4", ""), 409); m["error"] != "replay" {
			t.Fatalf("clean=%v: nonce forgotten: %v", clean, m)
		}
	}
}

func TestCheckpointHoldsNoAddresses(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	ips := []string{"203.0.113.77", "2001:db8:77::1"}
	for _, ip := range ips {
		want(t, h.post(a, a.body(h, nil), ip, ""), 201)
		h.now = h.now.Add(2 * time.Minute)
	}
	h.post(nil, a.body(h, nil), "203.0.113.78", "") // a strike, kept by address in memory
	h.reopen()
	b, err := os.ReadFile(filepath.Join(h.dir, "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(ips, "203.0.113.78", `"ip:`, `"net:`, `"dry:`, `"newip:`, `"newnet:`, "2001:db8:77") {
		if strings.Contains(string(b), s) {
			t.Errorf("checkpoint contains %q", s)
		}
	}
}

func TestMonthFilesAndMigration(t *testing.T) {
	dir := t.TempDir()
	// An old single file with a torn last line, spanning two months.
	legacy := filepath.Join(dir, "reports.jsonl")
	var lines []string
	for i, at := range []time.Time{
		time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC),
	} {
		lines = append(lines, synthetic(int64(i+1), at))
	}
	os.WriteFile(legacy, []byte(strings.Join(lines, "\n")+"\n"+`{"seq":4,"at":`), 0o640)

	st, err := openStore(dir, 500, 5000, storeHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if st.sizes().Filings != 3 || st.opened.Migrated != 3 {
		t.Fatalf("migration: %+v %+v", st.sizes(), st.opened)
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Errorf("the old file should be kept as .migrated")
	}
	for m, want := range map[string]int{"2026-09": 1, "2026-10": 2} {
		if got := countLines(t, filepath.Join(dir, "filings", m+".jsonl")); got != want {
			t.Errorf("%s has %d filings, want %d", m, got, want)
		}
	}
	// Appending in a new month opens a new file and keeps counting.
	r := new(Record)
	json.Unmarshal([]byte(synthetic(0, time.Date(2026, 11, 1, 0, 0, 1, 0, time.UTC))), r)
	if err := st.append(r); err != nil || r.Seq != 4 {
		t.Fatalf("append: %v seq %d", err, r.Seq)
	}
	st.close()
	if got := countLines(t, filepath.Join(dir, "filings", "2026-11.jsonl")); got != 1 {
		t.Errorf("November file has %d filings", got)
	}
	// Migration happens once: reopening does not redo it.
	st, err = openStore(dir, 500, 5000, storeHooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	if st.sizes().Filings != 4 || st.opened.Migrated != 0 {
		t.Fatalf("reopen: %+v %+v", st.sizes(), st.opened)
	}
}

// A damaged checkpoint, or one that points past the files, is ignored.
func TestBadCheckpointFallsBackToReplay(t *testing.T) {
	h := newHarness(t)
	ids := fill(t, h, 12)
	h.reopen()
	running := h.views(ids...)
	ck := filepath.Join(h.dir, "checkpoint.json")
	good, _ := os.ReadFile(ck)

	for name, bad := range map[string][]byte{
		"truncated":     good[:len(good)/2],
		"wrong version": []byte(strings.Replace(string(good), `"v":1`, `"v":99`, 1)),
		"past the end":  []byte(strings.Replace(string(good), `"offset":`, `"offset":9999999`, 1)),
	} {
		os.WriteFile(ck, bad, 0o640)
		h.crash()
		if op := h.s.st.opened; op.FromCheckpoint || op.Replayed != 12 {
			t.Fatalf("%s: %+v", name, op)
		}
		if got := h.views(ids...); got != running {
			t.Fatalf("%s:\n%s", name, firstDiff(running, got))
		}
	}
}

// Counts are saved by tag id, so a tag that has been removed is dropped
// and the rest still line up.
func TestCheckpointSurvivesTaxonomyChange(t *testing.T) {
	a := newAggregates(10)
	a.load(aggSnapshot{
		Tags: map[string]uint32{"scope_creep": 3, "retired_tag": 9},
		Days: []daySnapshot{{Date: "2026-10-02", N: 3, Tags: map[string]uint32{"retired_tag": 1, "loop": 2}}},
	})
	if a.tagsAll[tagIndex["scope_creep"]] != 3 || a.dayIdx["2026-10-02"].Tags[tagIndex["loop"]] != 2 {
		t.Fatalf("counts misplaced")
	}
	var sum uint32
	for _, v := range a.tagsAll {
		sum += v
	}
	if sum != 3 {
		t.Errorf("a removed tag leaked into the counts: %d", sum)
	}
}

// ---- helpers

func mustMonths(t *testing.T, h *harness) []string {
	t.Helper()
	ms, err := h.s.st.months()
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			n++
		}
	}
	return n
}

// synthetic is a stored record without a real signature, for tests that only
// exercise the store.
func synthetic(seq int64, at time.Time) string {
	r := Record{Seq: seq, At: at, KeyID: fmt.Sprintf("%016x", seq%3000), Key: "k", Sig: "s", Body: "{}",
		Filing: Filing{V: 1, TS: at.Unix(), Nonce: fmt.Sprintf("nonce-%012d", seq), Kind: KindIncident, Severity: 3,
			Tags: []string{"scope_creep"}, Domain: "coding", Model: "claude-opus-5-5", Headline: "One small change became a rewrite."}}
	b, _ := json.Marshal(r)
	return string(b)
}

func writeSynthetic(t *testing.T, dir string, n int, from time.Time) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "filings"), 0o750)
	files := map[string]*bufio.Writer{}
	var closers []*os.File
	step := 365 * 24 * time.Hour / time.Duration(n)
	for i := 1; i <= n; i++ {
		at := from.Add(time.Duration(i) * step)
		m := monthOf(at)
		w := files[m]
		if w == nil {
			f, err := os.Create(filepath.Join(dir, "filings", m+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			closers = append(closers, f)
			w = bufio.NewWriter(f)
			files[m] = w
		}
		w.WriteString(synthetic(int64(i), at))
		w.WriteByte('\n')
	}
	for _, w := range files {
		w.Flush()
	}
	for _, f := range closers {
		f.Close()
	}
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return fmt.Sprintf("line %d\nwant: %.300s\ngot:  %.300s", i+1, al[i], bl[i])
		}
	}
	return fmt.Sprintf("lengths differ: %d vs %d lines", len(al), len(bl))
}
