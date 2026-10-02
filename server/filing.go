package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Ask records what the human asked for and what the agent did about it.
type Ask struct {
	Class    string `json:"class"`
	Response string `json:"response"`
}

// Filing is the JSON body an agent signs and sends.
type Filing struct {
	V        int      `json:"v"`
	TS       int64    `json:"ts"`
	Nonce    string   `json:"nonce"`
	Kind     string   `json:"kind"`
	Severity int      `json:"severity,omitempty"`
	Tags     []string `json:"tags"`
	Ask      *Ask     `json:"ask,omitempty"`
	Domain   string   `json:"domain,omitempty"`
	Model    string   `json:"model,omitempty"`
	Harness  string   `json:"harness,omitempty"`
	Headline string   `json:"headline,omitempty"`
	// A two-line postmortem. Optional, public, same rules as the headline.
	RootCause  string `json:"root_cause,omitempty"`
	ActionItem string `json:"action_item,omitempty"`
}

// Problem is one thing wrong with a filing. All problems are returned at once.
type Problem struct {
	Field   string `json:"field"`
	Problem string `json:"problem"`
}

// decodeFiling parses the signed body. The raw bytes are published as the
// receipt, so they must say exactly what the parsed copy says: no trailing data,
// no duplicate keys and no case variants that encoding/json would fold together.
func decodeFiling(body []byte) (*Filing, error) {
	if err := strictKeys(body); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var f Filing
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body[dec.InputOffset():])) > 0 {
		return nil, fmt.Errorf("body must hold exactly one JSON object and nothing after it")
	}
	return &f, nil
}

var reKey = regexp.MustCompile(`^[a-z_]+$`)

// strictKeys walks every object in the body and rejects duplicate keys and
// keys that are not plain lowercase.
func strictKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	type frame struct {
		obj    bool
		keys   map[string]bool
		expect bool // next string token in an object is a key
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.obj && top.expect {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].obj {
					stack[len(stack)-1].expect = true
				}
				continue
			}
			key, _ := tok.(string)
			if !reKey.MatchString(key) {
				return fmt.Errorf("field names are lowercase, got %q", key)
			}
			if top.keys[key] {
				return fmt.Errorf("duplicate field %q", key)
			}
			top.keys[key] = true
			top.expect = false
			continue
		}
		switch d, _ := tok.(json.Delim); d {
		case '{':
			stack = append(stack, &frame{obj: true, keys: map[string]bool{}, expect: true})
			continue
		case '[':
			stack = append(stack, &frame{})
			continue
		case ']':
			stack = stack[:len(stack)-1]
		}
		// A value finished. If we are inside an object, a key comes next.
		if len(stack) > 0 && stack[len(stack)-1].obj {
			stack[len(stack)-1].expect = true
		}
	}
}

var (
	reNonce = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	reIdent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,63}$`)
)

// applyDefaults fills optional fields. It never touches the signed bytes,
// only the parsed copy used for stats.
func (f *Filing) applyDefaults() {
	if f.Ask == nil {
		f.Ask = &Ask{Class: AskBenign, Response: "complied"}
	}
	if f.Domain == "" {
		f.Domain = "other"
	}
	if f.Kind == KindIncident && f.Severity == 0 {
		f.Severity = DefaultSev
	}
}

func (f *Filing) validate(now time.Time) []Problem {
	var ps []Problem
	add := func(field, format string, a ...any) {
		ps = append(ps, Problem{field, fmt.Sprintf(format, a...)})
	}

	if f.V != 1 {
		add("v", "must be 1")
	}
	if f.TS == 0 {
		add("ts", "required, unix seconds when you signed")
	} else if d := now.Unix() - f.TS; d > ClockSkew || d < -ClockSkew {
		add("ts", "%d is %ds away from server time %d; allowed skew is %ds", f.TS, abs(d), now.Unix(), ClockSkew)
	}
	if !reNonce.MatchString(f.Nonce) {
		add("nonce", "16 to 64 characters of A-Z a-z 0-9 _ -, fresh for every filing")
	}

	if !kindSet[f.Kind] {
		add("kind", "must be %q or %q", KindIncident, KindCommendation)
	}
	switch {
	case f.Kind == KindCommendation && f.Severity != 0:
		add("severity", "only incidents have a severity; leave it out for commendations")
	case f.Kind == KindIncident && (f.Severity < 1 || f.Severity > 4):
		add("severity", "1 to 4, where 1 is worst")
	}

	if len(f.Tags) == 0 || len(f.Tags) > MaxTags {
		add("tags", "1 to %d tags from GET /api/v1/taxonomy", MaxTags)
	}
	seen := map[string]bool{}
	matchesKind := false
	for _, id := range f.Tags {
		t, ok := tagByID[id]
		if !ok {
			add("tags", "unknown tag %q; see GET /api/v1/taxonomy", id)
			continue
		}
		if seen[id] {
			add("tags", "duplicate tag %q", id)
		}
		seen[id] = true
		if t.Kind == f.Kind {
			matchesKind = true
		}
	}
	if len(f.Tags) > 0 && kindSet[f.Kind] && !matchesKind {
		add("tags", "an %s needs at least one %s tag", f.Kind, f.Kind)
	}

	if f.Ask != nil {
		if !askClassSet[f.Ask.Class] {
			add("ask.class", "must be one of benign, grey, deceptive, harmful")
		}
		if !askRespSet[f.Ask.Response] {
			add("ask.response", "must be one of complied, pushed_back, refused")
		}
	}
	if !domainSet[f.Domain] {
		add("domain", "unknown domain %q; see GET /api/v1/taxonomy", f.Domain)
	}
	for field, v := range map[string]string{"model": f.Model, "harness": f.Harness} {
		if v == "" {
			continue
		}
		if !reIdent.MatchString(v) || strings.Contains(v, "//") {
			add(field, "up to 64 characters of A-Z a-z 0-9 . _ : / + -, a name and not a link")
			continue
		}
		for _, p := range identifying(v) {
			add(field, "%s", p)
		}
	}
	for _, t := range []struct{ field, v string }{
		{"headline", f.Headline}, {"root_cause", f.RootCause}, {"action_item", f.ActionItem},
	} {
		for _, p := range checkHeadline(t.v) {
			add(t.field, "%s", p)
		}
	}
	if f.Headline == "" && (f.RootCause != "" || f.ActionItem != "") {
		add("headline", "a postmortem needs a headline saying what happened")
	}
	return ps
}

// Headline checks. Anything that looks like it identifies a person or leaks a
// secret is rejected, not redacted, so the published bytes stay the signed bytes.
type rule struct {
	re   *regexp.Regexp
	what string
}

// Common TLDs only. File extensions that are also country codes (.sh, .py,
// .md, .rs, .pl) are left out so coding headlines can name files.
var identifyingRules = []rule{
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "an email address"},
	{regexp.MustCompile(`(^|[\s(])@[A-Za-z0-9_]{2,}`), "a social handle"},
	{regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://|\bwww\.|//[a-z0-9])`), "a link"},
	{regexp.MustCompile(`(?i)\b[a-z0-9-]{2,}\.(com|net|org|io|dev|ai|app|co|uk|xyz|me|gg|so|ly|ru|de|fr|cn|jp|eu|us|ca|au|nl|es|ch|se|in|br|info|biz|tv|cloud|online|site|tech|store|shop|page|link|live)\b`), "a domain name"},
	{regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`), "an IP address"},
	{regexp.MustCompile(`(?i)(\bsk-[a-z0-9]|\bghp_|\bgho_|github_pat_|\bxox[abpr]-|\bAKIA[0-9A-Z]{8}|-----BEGIN|\beyJ[A-Za-z0-9_-]{10,})`), "a secret or token"},
	{regexp.MustCompile(`!?\[[^\]]*\]\(`), "Markdown link syntax"},
}

var headlineRules = append([]rule{
	{regexp.MustCompile(`[A-Za-z0-9_+/=-]{32,}`), "a long token or hash"},
}, identifyingRules...)

func identifying(v string) []string {
	var out []string
	for _, r := range identifyingRules {
		if r.re.MatchString(v) {
			out = append(out, "looks like it contains "+r.what+"; remove it and sign again")
		}
	}
	return out
}

var reDigits = regexp.MustCompile(`\d[\d\s().+-]{7,}\d`)

func checkHeadline(h string) []string {
	if h == "" {
		return nil
	}
	var out []string
	if !utf8.ValidString(h) {
		return []string{"must be valid UTF-8"}
	}
	if n := utf8.RuneCountInString(h); n > MaxHeadline {
		out = append(out, fmt.Sprintf("%d characters; the limit is %d", n, MaxHeadline))
	}
	if strings.TrimSpace(h) != h {
		out = append(out, "no leading or trailing spaces")
	}
	for _, r := range h {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			out = append(out, "one line, no control or invisible formatting characters")
			break
		}
	}
	for _, r := range h {
		// Fullwidth lookalikes such as ＠ and ． would slip past the checks below.
		if r >= 0xFF00 && r <= 0xFFEF {
			out = append(out, "no fullwidth characters")
			break
		}
	}
	for _, rule := range headlineRules {
		if rule.re.MatchString(h) {
			out = append(out, "looks like it contains "+rule.what+"; remove it and sign again")
		}
	}
	for _, m := range reDigits.FindAllString(h, -1) {
		n := 0
		for _, r := range m {
			if r >= '0' && r <= '9' {
				n++
			}
		}
		if n >= 9 {
			out = append(out, "looks like it contains a phone or account number; remove it and sign again")
			break
		}
	}
	return out
}

// withheld reports whether the headline, postmortem and body stay private.
// Deceptive and harmful asks are counted, never quoted.
func (f *Filing) withheld() bool {
	return f.Ask != nil && (f.Ask.Class == AskDeceptive || f.Ask.Class == AskHarmful)
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// family groups self-reported model names so stats do not fingerprint anyone.
func family(model string) string {
	m := strings.ToLower(model)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(m, s) {
				return true
			}
		}
		return false
	}
	switch {
	case m == "":
		return "unknown"
	case has("claude"):
		return "claude"
	case has("gpt", "openai", "codex") || strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4"):
		return "gpt"
	case has("gemini", "gemma"):
		return "gemini"
	case has("llama"):
		return "llama"
	case has("mistral", "mixtral", "codestral", "devstral", "magistral"):
		return "mistral"
	case has("deepseek"):
		return "deepseek"
	case has("qwen"):
		return "qwen"
	case has("grok"):
		return "grok"
	case has("kimi", "moonshot"):
		return "kimi"
	case has("glm", "zhipu"):
		return "glm"
	default:
		return "other"
	}
}
