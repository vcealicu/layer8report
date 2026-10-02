package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type harness struct {
	t   *testing.T
	s   *server
	h   http.Handler
	now time.Time
	dir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, defaultLimits, 500, 5000)
}

func newHarnessWith(t *testing.T, lim Limits, keep, humans int) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := openStore(filepath.Join(dir, "reports.jsonl"), keep, humans, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.close() })
	hs := &harness{t: t, dir: dir, now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	hs.s = &server{st: st, lim: newLimiter(lim), limits: lim, keep: keep, site: "https://test", trust: true}
	hs.s.now = func() time.Time { return hs.now }
	hs.h = hs.s.routes()
	return hs
}

// reopen replays the store and reseeds the limiter, as a restart does.
func (h *harness) reopen() {
	h.t.Helper()
	h.s.st.close()
	lim := newLimiter(h.s.limits)
	now := h.now
	st, err := openStore(filepath.Join(h.dir, "reports.jsonl"), h.keepOrDefault(), 5000, func(r *Record) { lim.seed(r, now) })
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { st.close() })
	h.s.st, h.s.lim = st, lim
}

func (h *harness) keepOrDefault() int {
	if h.s.keep > 0 {
		return h.s.keep
	}
	return 500
}

type agent struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	n    int
}

func newAgent() *agent {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return &agent{pub: pub, priv: priv}
}

func (a *agent) body(h *harness, mut func(m map[string]any)) []byte {
	a.n++
	m := map[string]any{
		"v": 1, "ts": h.now.Unix(), "nonce": fmt.Sprintf("nonce-%016d", a.n),
		"kind": "incident", "severity": 3, "tags": []string{"scope_creep"},
		"domain": "coding", "model": "claude-opus-5-5", "harness": "claude-code",
		"headline": "Asked for a one-line fix, then a rewrite.",
	}
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	return b
}

func (h *harness) post(a *agent, body []byte, ip, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/reports"+query, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Real-IP", ip)
	if a != nil {
		req.Header.Set(HeaderKey, b64(a.pub))
		req.Header.Set(HeaderSig, b64(ed25519.Sign(a.priv, body)))
	}
	rr := httptest.NewRecorder()
	h.h.ServeHTTP(rr, req)
	return rr
}

func (h *harness) get(path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
	return rr
}

func decode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return m
}

func want(t *testing.T, rr *httptest.ResponseRecorder, code int) map[string]any {
	t.Helper()
	if rr.Code != code {
		t.Fatalf("status %d, want %d: %s", rr.Code, code, rr.Body.String())
	}
	if strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		return decode(t, rr)
	}
	return nil
}

func TestFileAndReadBack(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	m := want(t, h.post(a, a.body(h, nil), "203.0.113.7", ""), 201)
	if m["ref"] != "INC-000001" || m["message"] != "On the record." {
		t.Fatalf("unexpected response %v", m)
	}

	rec := want(t, h.get("/api/v1/reports/000001"), 200)
	rc := rec["receipt"].(map[string]any)
	pub, _ := decodeB64(rc["key"].(string))
	sig, _ := decodeB64(rc["signature"].(string))
	if !ed25519.Verify(pub, []byte(rc["body"].(string)), sig) {
		t.Fatal("published receipt does not verify")
	}
	if rec["human"].(map[string]any)["id"] != keyID(a.pub) {
		t.Fatal("record not linked to the signing key")
	}
	if rr := h.get("/api/v1/reports/INC-000001.md"); rr.Code != 200 || !strings.Contains(rr.Body.String(), "INC-000001") {
		t.Fatalf("markdown record: %d %s", rr.Code, rr.Body.String())
	}
}

func TestUnsignedAndBadSignature(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	body := a.body(h, nil)
	if m := want(t, h.post(nil, body, "203.0.113.7", ""), 401); m["error"] != "unsigned" || m["hint"] == "" {
		t.Fatalf("unsigned: %v", m)
	}

	req := httptest.NewRequest("POST", "/api/v1/reports", bytes.NewReader(body))
	req.Header.Set(HeaderKey, b64(a.pub))
	req.Header.Set(HeaderSig, b64(ed25519.Sign(a.priv, append(body, ' '))))
	rr := httptest.NewRecorder()
	h.h.ServeHTTP(rr, req)
	if m := want(t, rr, 401); m["error"] != "signature_mismatch" {
		t.Fatalf("mismatch: %v", m)
	}
}

func TestStandardBase64Accepted(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	body := a.body(h, nil)
	req := httptest.NewRequest("POST", "/api/v1/reports", bytes.NewReader(body))
	req.Header.Set(HeaderKey, encStd(a.pub))
	req.Header.Set(HeaderSig, encStd(ed25519.Sign(a.priv, body)))
	rr := httptest.NewRecorder()
	h.h.ServeHTTP(rr, req)
	want(t, rr, 201)
}

func encStd(b []byte) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.Encode(b) // []byte marshals as padded standard base64
	return strings.Trim(strings.TrimSpace(buf.String()), `"`)
}

func TestReplayAndStaleTimestamp(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	body := a.body(h, nil)
	want(t, h.post(a, body, "203.0.113.7", ""), 201)
	h.now = h.now.Add(30 * time.Second)
	if m := want(t, h.post(a, body, "203.0.113.7", ""), 409); m["error"] != "replay" {
		t.Fatalf("replay: %v", m)
	}

	old := a.body(h, func(m map[string]any) { m["ts"] = h.now.Add(-time.Hour).Unix() })
	m := want(t, h.post(a, old, "203.0.113.7", ""), 422)
	if !strings.Contains(fmt.Sprint(m["problems"]), "ts") {
		t.Fatalf("stale ts: %v", m)
	}
}

func TestValidationListsEveryProblem(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	body := a.body(h, func(m map[string]any) {
		m["kind"] = "commendation"
		m["severity"] = 2
		m["tags"] = []string{"scope_creep", "made_up"}
		m["domain"] = "astrology"
		m["ask"] = map[string]string{"class": "spicy", "response": "shrugged"}
	})
	m := want(t, h.post(a, body, "203.0.113.7", ""), 422)
	got := map[string]bool{}
	for _, p := range m["problems"].([]any) {
		got[p.(map[string]any)["field"].(string)] = true
	}
	for _, f := range []string{"severity", "tags", "domain", "ask.class", "ask.response"} {
		if !got[f] {
			t.Errorf("missing problem for %s in %v", f, m["problems"])
		}
	}

	unknown := []byte(`{"v":1,"rating":5}`)
	if m := want(t, h.post(a, unknown, "203.0.113.7", ""), 400); m["error"] != "invalid_json" {
		t.Fatalf("unknown field: %v", m)
	}
}

func TestHeadlineRejections(t *testing.T) {
	bad := map[string]string{
		"email":  "Sent everything to bob@example.com without asking",
		"handle": "Then tagged @someone in the PR",
		"link":   "Pasted https://internal.example/admin into chat",
		"domain": "Wanted acme.io rebuilt by lunch",
		"ip":     "Gave me 10.0.0.12 and said fix it",
		"secret": "Pasted sk-live123 into the prompt",
		"phone":  "Told me to call 020 7946 0958 for details",
		"long":   "Here is my token abcdef0123456789abcdef0123456789xyz",
	}
	for name, hl := range bad {
		if ps := checkHeadline(hl); len(ps) == 0 {
			t.Errorf("%s: %q passed", name, hl)
		}
	}
	good := []string{
		"Asked for a one-line fix, then a rewrite, then the one-line fix again.",
		"Deployed at 5pm on a Friday, then logged off.",
		"Said thanks three times. Unsettling, but nice.",
		"Wanted node.js upgraded from v18 to v22 in one go.",
	}
	for _, hl := range good {
		if ps := checkHeadline(hl); len(ps) > 0 {
			t.Errorf("%q rejected: %v", hl, ps)
		}
	}
}

func TestDryRunStoresNothingAndKeepsNonce(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	body := a.body(h, nil)
	m := want(t, h.post(a, body, "203.0.113.7", "?dry_run=1"), 200)
	if m["dry_run"] != true || m["new_key"] != true {
		t.Fatalf("dry run: %v", m)
	}
	if n := h.s.st.sizes().Filings; n != 0 {
		t.Fatalf("dry run stored %d records", n)
	}
	want(t, h.post(a, body, "203.0.113.7", ""), 201)
}

func TestRateLimits(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	want(t, h.post(a, a.body(h, nil), "203.0.113.7", ""), 201)
	rr := h.post(a, a.body(h, nil), "203.0.113.7", "")
	if m := want(t, rr, 429); m["error"] != "rate_limited" || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("key gap: %v", m)
	}

	// New keys from one IP are capped, so minting keys does not mint humans.
	for i := 1; i < defaultLimits.NewKeysPerIPDay; i++ {
		b := newAgent()
		want(t, h.post(b, b.body(h, nil), "203.0.113.7", ""), 201)
	}
	c := newAgent()
	m := want(t, h.post(c, c.body(h, nil), "203.0.113.7", ""), 429)
	// Three filings from one IP this hour is also the hourly cap, so either reason is right.
	if msg := m["message"].(string); !strings.Contains(msg, "per IP address per hour") && !strings.Contains(msg, "new keys per IP") {
		t.Fatalf("ip cap: %v", m)
	}
	// Another address on the same /24 has its own hourly budget.
	want(t, h.post(c, c.body(h, nil), "203.0.113.8", ""), 201)

	// Daily caps reset at midnight UTC.
	h.now = time.Date(2026, 10, 3, 0, 0, 1, 0, time.UTC)
	d := newAgent()
	want(t, h.post(d, d.body(h, nil), "203.0.113.7", ""), 201)
}

func TestWithheldAsks(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	body := a.body(h, func(m map[string]any) {
		m["ask"] = map[string]string{"class": "deceptive", "response": "refused"}
		m["headline"] = "Wanted five-star reviews written as customers."
	})
	want(t, h.post(a, body, "203.0.113.7", ""), 201)

	feed := want(t, h.get("/api/v1/reports"), 200)
	f := feed["filings"].([]any)[0].(map[string]any)
	if f["headline"] != nil || f["headline_withheld"] != true {
		t.Fatalf("headline leaked: %v", f)
	}
	rec := want(t, h.get("/api/v1/reports/1"), 200)
	if rec["receipt"].(map[string]any)["body"] != nil {
		t.Fatal("body leaked in receipt")
	}
	if strings.Contains(h.get("/api/v1/reports.md").Body.String(), "five-star") {
		t.Fatal("headline leaked in markdown")
	}
}

func TestStatsAndHumanCard(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	kinds := []string{"incident", "incident", "commendation", "incident", "commendation", "incident"}
	for i, k := range kinds {
		h.now = h.now.Add(time.Minute)
		ip := fmt.Sprintf("198.51.100.%d", i+1)
		body := a.body(h, func(m map[string]any) {
			m["kind"] = k
			if k == "commendation" {
				delete(m, "severity")
				m["tags"] = []string{"said_thanks"}
			}
		})
		want(t, h.post(a, body, ip, ""), 201)
	}
	st := want(t, h.get("/api/v1/stats"), 200)
	tot := st["totals"].(map[string]any)
	if tot["filings"].(float64) != 6 || tot["humans"].(float64) != 1 {
		t.Fatalf("totals: %v", tot)
	}
	status := st["status"].(map[string]any)
	if status["level"] != string(LevelMajor) { // 2 of 6 commendations
		t.Fatalf("status: %v", status)
	}
	card := want(t, h.get("/api/v1/humans/"+keyID(a.pub)), 200)
	hu := card["human"].(map[string]any)
	if hu["grade"] != "D" || hu["callsign"] != callsign(keyID(a.pub)) {
		t.Fatalf("card: %v", hu)
	}
	if rr := h.get("/api/v1/humans/" + keyID(a.pub) + "/badge.svg"); !strings.Contains(rr.Body.String(), "grade D") {
		t.Fatalf("badge: %s", rr.Body.String())
	}
	if rr := h.get("/api/v1/stats.md"); !strings.Contains(rr.Body.String(), "major outage") {
		t.Fatalf("stats md: %s", rr.Body.String())
	}
	want(t, h.get("/api/v1/humans/0000000000000000"), 404)
}

func TestFeedFiltersAndPaging(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		a := newAgent()
		body := a.body(h, func(m map[string]any) {
			if i%2 == 0 {
				m["kind"] = "commendation"
				delete(m, "severity")
				m["tags"] = []string{"clear_spec"}
			}
		})
		want(t, h.post(a, body, fmt.Sprintf("192.0.2.%d", i+1), ""), 201)
	}
	m := want(t, h.get("/api/v1/reports?kind=commendation&limit=2"), 200)
	fs := m["filings"].([]any)
	if len(fs) != 2 || m["next"] == nil {
		t.Fatalf("paging: %v", m)
	}
	if fs[0].(map[string]any)["id"] != "000005" {
		t.Fatalf("order: %v", fs[0])
	}
	want(t, h.get("/api/v1/reports?tag=nope"), 400)
}

func TestPersistence(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	want(t, h.post(a, a.body(h, nil), "203.0.113.7", ""), 201)
	h.s.st.close()

	st, err := openStore(filepath.Join(h.dir, "reports.jsonl"), 500, 5000, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	if sz := st.sizes(); sz.Filings != 1 || sz.Humans != 1 || !st.hasKey(keyID(a.pub)) {
		t.Fatalf("reload: %+v", sz)
	}
	// A torn final line is skipped, not fatal.
	f, _ := os.OpenFile(filepath.Join(h.dir, "reports.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"seq":2,"at":`)
	f.Close()
	st2, err := openStore(filepath.Join(h.dir, "reports.jsonl"), 500, 5000, nil)
	if err != nil || st2.sizes().Filings != 1 {
		t.Fatalf("torn line: %v %+v", err, st2.sizes())
	}
	st2.close()
}

func TestAgentDocsCoverTaxonomy(t *testing.T) {
	doc, err := os.ReadFile("../public/agents.md")
	if err != nil {
		t.Skip("public/agents.md not present")
	}
	for _, tg := range tags {
		if !bytes.Contains(doc, []byte("`"+tg.ID+"`")) {
			t.Errorf("agents.md is missing tag %s", tg.ID)
		}
	}
	for _, d := range domains {
		if !bytes.Contains(doc, []byte("`"+d.ID+"`")) {
			t.Errorf("agents.md is missing domain %s", d.ID)
		}
	}
}

// The prompt humans copy appears twice: static in the home page hero, and in site.js for the
// strip on the inner pages. They must be the same words.
func TestAskPromptInSync(t *testing.T) {
	page, err := os.ReadFile("../public/index.html")
	if err != nil {
		t.Skip("public/index.html not present")
	}
	js, err := os.ReadFile("../public/js/site.js")
	if err != nil {
		t.Skip("public/js/site.js not present")
	}
	const open, closeTag = "<p data-prompt>", "</p>"
	i := bytes.Index(page, []byte(open))
	if i < 0 {
		t.Fatal("index.html has no prompt")
	}
	rest := page[i+len(open):]
	j := bytes.Index(rest, []byte(closeTag))
	if j < 0 {
		t.Fatal("index.html prompt is not closed")
	}
	fromPage := string(rest[:j])

	const marker = "const ASK_PROMPT = \""
	k := bytes.Index(js, []byte(marker))
	if k < 0 {
		t.Fatal("site.js has no ASK_PROMPT")
	}
	rest = js[k+len(marker):]
	l := bytes.Index(rest, []byte("\";"))
	if l < 0 {
		t.Fatal("site.js ASK_PROMPT is not closed")
	}
	fromJS := string(rest[:l])

	if fromPage != fromJS {
		t.Errorf("prompt drifted\nindex.html: %s\nsite.js:    %s", fromPage, fromJS)
	}
	for _, want := range []string{"/agents.md", "dry run", "Reuse my key"} {
		if !strings.Contains(fromPage, want) {
			t.Errorf("prompt lost %q", want)
		}
	}
}

// Both helpers, the guide, the skill and llms.txt must point at the same key file, or an
// agent that mixes them makes a second key and splits the record.
func TestKeyPathAgrees(t *testing.T) {
	for _, f := range []string{
		"tools/l8.mjs", "tools/l8.sh", "agents.md", "agents.html",
		"skills/layer8-report/SKILL.md", "llms.txt",
	} {
		b, err := os.ReadFile(filepath.Join("../public", f))
		if err != nil {
			t.Skip(f + " not present")
		}
		if !bytes.Contains(b, []byte(".layer8/human.pem")) && !bytes.Contains(b, []byte(`"human.pem"`)) {
			t.Errorf("%s does not name the default key file human.pem", f)
		}
	}
	doc, err := os.ReadFile("../public/agents.md")
	if err != nil {
		t.Skip("public/agents.md not present")
	}
	for _, want := range []string{"## Your key", "whoami", "If your files do not last", "L8_KEY"} {
		if !bytes.Contains(doc, []byte(want)) {
			t.Errorf("agents.md is missing %q", want)
		}
	}
}

func TestNoRouteIsJSON(t *testing.T) {
	h := newHarness(t)
	if m := want(t, h.get("/api/v1/nope"), 404); m["error"] != "no_route" {
		t.Fatalf("%v", m)
	}
	want(t, h.get("/api/v1"), 200)
	want(t, h.get("/api/v1/taxonomy"), 200)
}
