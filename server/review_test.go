package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for issues found in review.

func TestRejectsTrailingDataAndKeyTricks(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	good := a.body(h, nil)
	cases := map[string][]byte{
		"trailing brace":   append(append([]byte{}, good...), []byte(`} bob@example.com`)...),
		"second object":    append(append([]byte{}, good...), []byte(`{"v":1}`)...),
		"duplicate key":    []byte(strings.Replace(string(good), `"v":1`, `"headline":"bob@example.com","v":1`, 1)),
		"case variant key": []byte(strings.Replace(string(good), `"v":1`, `"HEADLINE":"fine","v":1`, 1)),
		"nested duplicate": []byte(strings.Replace(string(good), `"v":1`, `"ask":{"class":"benign","class":"grey","response":"complied"},"v":1`, 1)),
	}
	for name, body := range cases {
		rr := h.post(a, body, "203.0.113.7", "")
		if rr.Code != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, rr.Code, rr.Body.String())
		}
	}
	want(t, h.post(a, good, "203.0.113.7", ""), 201)
}

func TestModelAndHarnessCannotCarryContacts(t *testing.T) {
	h := newHarness(t)
	for _, v := range []string{"bob@example.com", "https://evil.example/x", "acme.com", "a//b"} {
		a := newAgent()
		body := a.body(h, func(m map[string]any) { m["harness"] = v })
		if rr := h.post(a, body, "203.0.113.7", "?dry_run=1"); rr.Code != 422 {
			t.Errorf("harness %q: status %d", v, rr.Code)
		}
	}
	a := newAgent()
	ok := a.body(h, func(m map[string]any) {
		m["model"] = "openrouter/meta-llama/llama-3.1-405b-instruct"
		m["harness"] = "claude-code"
	})
	want(t, h.post(a, ok, "203.0.113.7", "?dry_run=1"), 200)
}

func TestReplayBlockedAfterRestart(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	body := a.body(h, nil)
	want(t, h.post(a, body, "203.0.113.7", ""), 201)
	h.now = h.now.Add(time.Minute)
	h.reopen()
	if m := want(t, h.post(a, body, "198.51.100.1", ""), 409); m["error"] != "replay" {
		t.Fatalf("replay after restart: %v", m)
	}
}

func TestTornLineDoesNotEatNextRecord(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	want(t, h.post(a, a.body(h, nil), "203.0.113.7", ""), 201)
	h.s.st.close()
	path := filepath.Join(h.dir, "reports.jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"seq":2,"at":"2026-`)
	f.Close()

	h.now = h.now.Add(time.Minute)
	h.reopen()
	want(t, h.post(a, a.body(h, nil), "203.0.113.7", ""), 201)
	h.s.st.close()

	st2, err := openStore(path, 500, 5000, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.close()
	if sz := st2.sizes(); sz.Filings != 2 {
		t.Fatalf("got %d records after torn line, want 2", sz.Filings)
	}
}

func TestSmallOrderKeysRejected(t *testing.T) {
	weak := []string{
		"0000000000000000000000000000000000000000000000000000000000000000",
		"0100000000000000000000000000000000000000000000000000000000000000",
		"0100000000000000000000000000000000000000000000000000000000000080",
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05",
		"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85",
		"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a",
		"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa",
	}
	for _, hx := range weak {
		b, _ := hex.DecodeString(hx)
		if _, err := parseKey(b64(b)); err != errWeakKey {
			t.Errorf("%s accepted (%v)", hx, err)
		}
	}
	for i := 0; i < 50; i++ {
		if _, err := parseKey(b64(newAgent().pub)); err != nil {
			t.Fatalf("real key rejected: %v", err)
		}
	}
}

func TestHeadlineInjectionAndLookalikes(t *testing.T) {
	for _, hl := range []string{
		"![x](//evil.de/p.png)",
		"see [this](somewhere)",
		"mail bob＠example．com",
		"reversed ‮text",
		"zero​width",
		"visit evil.ru today",
	} {
		if len(checkHeadline(hl)) == 0 {
			t.Errorf("%q passed", hl)
		}
	}
	for _, hl := range []string{"Renamed main.go, deploy.sh and README.md in one commit.", "Wanted the tests in test_api.py rewritten in Rust."} {
		if ps := checkHeadline(hl); len(ps) > 0 {
			t.Errorf("%q rejected: %v", hl, ps)
		}
	}
	if got := mdCell("**bold** <b> [a](b) `x`"); strings.Contains(got, "**bold**") || strings.Contains(got, "<b>") || strings.Contains(got, "[a](b)") {
		t.Errorf("mdCell did not escape: %s", got)
	}
	if f := fence("a ```` b"); f != "`````" {
		t.Errorf("fence %q", f)
	}
}

func TestUnknownFamilyAndFarRecordAreCheap(t *testing.T) {
	h := newHarness(t)
	want(t, h.get("/api/v1/reports?family=nope"), 400)
	want(t, h.get("/api/v1/reports/999999999"), 404)
	want(t, h.get("/api/v1/reports/0"), 404)
}

func TestIPv6CountsPer64(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < defaultLimits.NewKeysPerIPDay; i++ {
		a := newAgent()
		want(t, h.post(a, a.body(h, nil), fmt.Sprintf("2001:db8:1:2::%x", i+1), ""), 201)
	}
	a := newAgent()
	m := want(t, h.post(a, a.body(h, nil), "2001:db8:1:2::ff", ""), 429)
	// The whole /64 shares one address budget, so either the hourly cap or the new-key cap trips.
	if msg := m["message"].(string); !strings.Contains(msg, "per IP address per hour") && !strings.Contains(msg, "new keys per IP") {
		t.Fatalf("%v", m)
	}
	// A different /64 in the same /48 is a different address.
	b := newAgent()
	want(t, h.post(b, b.body(h, nil), "2001:db8:1:3::1", ""), 201)
}

func TestEmptyStatsHaveArrays(t *testing.T) {
	h := newHarness(t)
	body := h.get("/api/v1/stats").Body.String()
	for _, k := range []string{`"families": []`, `"domains": []`} {
		if !strings.Contains(body, k) {
			t.Errorf("empty stats missing %s", k)
		}
	}
}
