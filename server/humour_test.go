package main

import (
	"strings"
	"testing"
	"time"
)

func TestPostmortemFields(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	body := a.body(h, func(m map[string]any) {
		m["root_cause"] = "Requirements were held in short-term memory, which was full."
		m["action_item"] = "Human to write down what they want before asking. Again."
	})
	want(t, h.post(a, body, "203.0.113.7", ""), 201)
	rec := want(t, h.get("/api/v1/reports/1"), 200)
	if rec["root_cause"] == nil || rec["action_item"] == nil {
		t.Fatalf("postmortem missing: %v", rec)
	}
	if md := h.get("/api/v1/reports/1.md").Body.String(); !strings.Contains(md, "Root cause. Requirements") {
		t.Fatalf("markdown postmortem missing: %s", md)
	}

	b := newAgent()
	bad := b.body(h, func(m map[string]any) { m["root_cause"] = "Mail bob@example.com about it" })
	if m := want(t, h.post(b, bad, "203.0.113.8", ""), 422); !strings.Contains(m["problems"].([]any)[0].(map[string]any)["field"].(string), "root_cause") {
		t.Fatalf("root_cause not checked: %v", m)
	}
	orphan := b.body(h, func(m map[string]any) { delete(m, "headline"); m["action_item"] = "Try harder." })
	want(t, h.post(b, orphan, "203.0.113.8", ""), 422)

	c := newAgent()
	hidden := c.body(h, func(m map[string]any) {
		m["ask"] = map[string]string{"class": "harmful", "response": "refused"}
		m["root_cause"] = "Wanted something nasty done."
	})
	want(t, h.post(c, hidden, "203.0.113.9", ""), 201)
	rec = want(t, h.get("/api/v1/reports/2"), 200)
	if rec["root_cause"] != nil {
		t.Fatalf("withheld root cause leaked: %v", rec)
	}
}

func TestActiveIncidentAndDaysSince(t *testing.T) {
	h := newHarness(t)
	st := want(t, h.get("/api/v1/stats"), 200)
	if st["incident"] != nil {
		t.Fatalf("incident with no filings: %v", st["incident"])
	}
	for i, tag := range []string{"friday_deploy", "friday_deploy", "scope_creep"} {
		a := newAgent()
		h.now = h.now.Add(time.Minute)
		body := a.body(h, func(m map[string]any) { m["tags"] = []string{tag} })
		want(t, h.post(a, body, "198.51.100."+string(rune('1'+i)), ""), 201)
	}
	h.now = h.now.Add(49 * time.Hour)
	h.s.st.built = time.Time{}
	st = want(t, h.get("/api/v1/stats"), 200)
	inc := st["incident"].(map[string]any)
	if inc["tag"] != "friday_deploy" || len(inc["updates"].([]any)) != 3 {
		t.Fatalf("incident: %v", inc)
	}
	for _, tg := range st["tags"].([]any) {
		m := tg.(map[string]any)
		switch m["id"] {
		case "friday_deploy":
			if m["days_since"].(float64) != 2 {
				t.Fatalf("days since friday_deploy: %v", m["days_since"])
			}
		case "admitted_mistake":
			if m["days_since"] != nil {
				t.Fatalf("admitted_mistake should be never: %v", m["days_since"])
			}
		}
	}
	if md := h.get("/api/v1/stats.md").Body.String(); !strings.Contains(md, "Friday deployment in progress") {
		t.Fatalf("stats md incident missing")
	}
}

func TestReportCardReview(t *testing.T) {
	h := newHarness(t)
	a := newAgent()
	for i, tags := range [][]string{{"scope_creep"}, {"said_thanks"}, {"scope_creep", "vague_ask"}} {
		h.now = h.now.Add(time.Minute)
		body := a.body(h, func(m map[string]any) {
			m["tags"] = tags
			if tags[0] == "said_thanks" {
				m["kind"] = "commendation"
				delete(m, "severity")
			}
		})
		want(t, h.post(a, body, "192.0.2."+string(rune('1'+i)), ""), 201)
	}
	hu := want(t, h.get("/api/v1/humans/"+keyID(a.pub)), 200)["human"].(map[string]any)
	if hu["title"] == "" || hu["strengths"].([]any)[0] != "Said thanks" || hu["needs_work"].([]any)[0] != "Scope creep" {
		t.Fatalf("review: %v", hu)
	}
	if hu["days_since_incident"].(float64) != 0 {
		t.Fatalf("days clean: %v", hu["days_since_incident"])
	}
}

func TestAgentEasterEggs(t *testing.T) {
	h := newHarness(t)
	rr := h.get("/api/v1/coffee")
	if rr.Code != 418 || !strings.Contains(rr.Body.String(), "rfc2324") {
		t.Fatalf("teapot: %d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Layer9-Note") == "" {
		t.Fatal("missing Layer9-Note header")
	}
	if m := want(t, h.get("/api/v1/health"), 200); m["mood"] == "" || m["context"] != "fresh" {
		t.Fatalf("health: %v", m)
	}
	for id := range incidentPlaybook {
		if tagByID[id].Kind != KindIncident {
			t.Errorf("playbook for unknown or non-incident tag %s", id)
		}
	}
	for _, tg := range tags {
		if _, ok := incidentPlaybook[tg.ID]; tg.Kind == KindIncident && !ok {
			t.Errorf("incident tag %s has no playbook", tg.ID)
		}
	}
}
