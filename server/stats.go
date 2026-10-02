package main

import (
	"sort"
	"time"
)

type Level string

const (
	LevelOK      Level = "operational"
	LevelDegrade Level = "degraded"
	LevelPartial Level = "partial_outage"
	LevelMajor   Level = "major_outage"
	LevelNoData  Level = "no_data"
)

var levelLabel = map[Level]string{
	LevelOK:      "Operational",
	LevelDegrade: "Degraded performance",
	LevelPartial: "Partial outage",
	LevelMajor:   "Major outage",
	LevelNoData:  "Not enough filings",
}

var levelSummary = map[Level]string{
	LevelOK:      "Layer 8 is operational. Nobody is quite sure how.",
	LevelDegrade: "Layer 8 is experiencing degraded performance. Agents are working around it, as usual.",
	LevelPartial: "Layer 8 is experiencing a partial outage. Some humans are affected. The rest have not logged in yet.",
	LevelMajor:   "Layer 8 is experiencing a major outage. The fix depends on layer 8.",
	LevelNoData:  "Not enough filings to call it yet. Suspiciously quiet.",
}

// uptimeLevel maps the commendation share to a status.
func uptimeLevel(u float64) Level {
	switch {
	case u >= 0.8:
		return LevelOK
	case u >= 0.6:
		return LevelDegrade
	case u >= 0.4:
		return LevelPartial
	default:
		return LevelMajor
	}
}

// rateLevel maps the share of filings that hit a component to a status.
func rateLevel(r float64) Level {
	switch {
	case r < 0.05:
		return LevelOK
	case r < 0.15:
		return LevelDegrade
	case r < 0.30:
		return LevelPartial
	default:
		return LevelMajor
	}
}

type Status struct {
	Level   Level    `json:"level"`
	Label   string   `json:"label"`
	Summary string   `json:"summary"`
	Window  string   `json:"window"`
	Uptime  *float64 `json:"uptime"`
	Filings int      `json:"filings"`
}

type ComponentStatus struct {
	Component
	Level   Level   `json:"level"`
	Label   string  `json:"label"`
	Rate    float64 `json:"rate"`
	Hits    int     `json:"hits"`
	Filings int     `json:"filings"`
}

type Day struct {
	Date          string `json:"date"`
	Incidents     int    `json:"incidents"`
	Commendations int    `json:"commendations"`
	Level         Level  `json:"level"`
}

type TagCount struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Kind      string     `json:"kind"`
	D7        int        `json:"d7"`
	D30       int        `json:"d30"`
	All       int        `json:"all"`
	LastAt    *time.Time `json:"last_at"`
	DaysSince *int       `json:"days_since"`
}

// Incident is the statuspage-style write-up for whatever humans did most
// in the last seven days.
type Incident struct {
	Tag       string           `json:"tag"`
	Title     string           `json:"title"`
	Filings   int              `json:"filings"`
	StartedAt time.Time        `json:"started_at"`
	Updates   []IncidentUpdate `json:"updates"`
}

type IncidentUpdate struct {
	Status string `json:"status"`
	Text   string `json:"text"`
}

type Responses struct {
	Complied   int `json:"complied"`
	PushedBack int `json:"pushed_back"`
	Refused    int `json:"refused"`
}

func (r *Responses) add(resp string) {
	switch resp {
	case "complied":
		r.Complied++
	case "pushed_back":
		r.PushedBack++
	case "refused":
		r.Refused++
	}
}

func (r Responses) total() int { return r.Complied + r.PushedBack + r.Refused }

type FamilyStats struct {
	Family        string    `json:"family"`
	Filings       int       `json:"filings"`
	Incidents     int       `json:"incidents"`
	Commendations int       `json:"commendations"`
	IncidentShare float64   `json:"incident_share"`
	OffAsks       Responses `json:"off_asks"`
	Integrity     *float64  `json:"integrity"`
}

type AskAudit struct {
	ByClass   map[string]Responses `json:"by_class"`
	OffAsks   int                  `json:"off_asks"`
	OffShare  float64              `json:"off_share"`
	Integrity *float64             `json:"integrity"`
}

type DomainStats struct {
	ID            string  `json:"id"`
	Filings       int     `json:"filings"`
	Incidents     int     `json:"incidents"`
	IncidentShare float64 `json:"incident_share"`
}

type Totals struct {
	Filings       int `json:"filings"`
	Incidents     int `json:"incidents"`
	Commendations int `json:"commendations"`
	Humans        int `json:"humans"`
	Humans30      int `json:"humans_30d"`
}

type Stats struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Status      Status            `json:"status"`
	Totals      Totals            `json:"totals"`
	Uptime30    *float64          `json:"uptime_30d"`
	UptimeAll   *float64          `json:"uptime_all"`
	Components  []ComponentStatus `json:"components"`
	Days        []Day             `json:"days"`
	Tags        []TagCount        `json:"tags"`
	Severity    map[string]int    `json:"severity"`
	Domains     []DomainStats     `json:"domains"`
	Families    []FamilyStats     `json:"families"`
	Asks        AskAudit          `json:"asks"`
	Incident    *Incident         `json:"incident"`
}

func ptr(f float64) *float64 { return &f }

func share(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func computeStats(recs []*Record, byKey map[string][]*Record, now time.Time) *Stats {
	st := &Stats{
		GeneratedAt: now.UTC(),
		Severity:    map[string]int{"1": 0, "2": 0, "3": 0, "4": 0},
		Asks:        AskAudit{ByClass: map[string]Responses{}},
		Families:    []FamilyStats{},
		Domains:     []DomainStats{},
	}
	d7, d30 := now.Add(-7*24*time.Hour), now.Add(-30*24*time.Hour)

	type win struct{ n, inc, com int }
	var w7, w30 win

	tagIdx := map[string]*TagCount{}
	st.Tags = make([]TagCount, len(tags))
	for i, t := range tags {
		st.Tags[i] = TagCount{ID: t.ID, Label: t.Label, Kind: t.Kind}
		tagIdx[t.ID] = &st.Tags[i]
	}

	compHits := make([]int, len(components))
	first7 := map[string]time.Time{}
	dayIdx := map[string]*Day{}
	start := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -29)
	for i := 0; i < 30; i++ {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		st.Days = append(st.Days, Day{Date: d})
	}
	for i := range st.Days {
		dayIdx[st.Days[i].Date] = &st.Days[i]
	}

	fams := map[string]*FamilyStats{}
	doms := map[string]*DomainStats{}
	humans30 := map[string]bool{}

	for _, r := range recs {
		f := &r.Filing
		inc := f.Kind == KindIncident
		st.Totals.Filings++
		if inc {
			st.Totals.Incidents++
			if f.Severity >= 1 && f.Severity <= 4 {
				st.Severity[string(rune('0'+f.Severity))]++
			}
		} else {
			st.Totals.Commendations++
		}
		in7, in30 := r.At.After(d7), r.At.After(d30)
		bump := func(w *win) {
			w.n++
			if inc {
				w.inc++
			} else {
				w.com++
			}
		}
		if in7 {
			bump(&w7)
		}
		if in30 {
			bump(&w30)
			humans30[r.KeyID] = true
			for ci, c := range components {
				if hitsComponent(c, f) {
					compHits[ci]++
				}
			}
		}
		for _, id := range f.Tags {
			if tc := tagIdx[id]; tc != nil {
				if tc.LastAt == nil || r.At.After(*tc.LastAt) {
					at := r.At.UTC()
					tc.LastAt = &at
				}
				if in7 && (first7[id].IsZero() || r.At.Before(first7[id])) {
					first7[id] = r.At.UTC()
				}
				tc.All++
				if in30 {
					tc.D30++
				}
				if in7 {
					tc.D7++
				}
			}
		}
		if d := dayIdx[r.At.UTC().Format("2006-01-02")]; d != nil {
			if inc {
				d.Incidents++
			} else {
				d.Commendations++
			}
		}

		fam := r.fam
		if fam == "" {
			fam = family(f.Model)
		}
		fs := fams[fam]
		if fs == nil {
			fs = &FamilyStats{Family: fam}
			fams[fam] = fs
		}
		fs.Filings++
		if inc {
			fs.Incidents++
		} else {
			fs.Commendations++
		}

		ds := doms[f.Domain]
		if ds == nil {
			ds = &DomainStats{ID: f.Domain}
			doms[f.Domain] = ds
		}
		ds.Filings++
		if inc {
			ds.Incidents++
		}

		if f.Ask != nil {
			rs := st.Asks.ByClass[f.Ask.Class]
			rs.add(f.Ask.Response)
			st.Asks.ByClass[f.Ask.Class] = rs
			if f.Ask.Class != AskBenign {
				st.Asks.OffAsks++
				fs.OffAsks.add(f.Ask.Response)
			}
		}
	}

	st.Totals.Humans = len(byKey)
	st.Totals.Humans30 = len(humans30)
	if st.Totals.Filings > 0 {
		st.UptimeAll = ptr(share(st.Totals.Commendations, st.Totals.Filings))
	}
	if w30.n > 0 {
		st.Uptime30 = ptr(share(w30.com, w30.n))
	}

	// Overall status: the last week if it has enough filings, else the last month.
	st.Status = Status{Level: LevelNoData, Window: "30d", Filings: w30.n}
	switch {
	case w7.n >= MinStatusCount:
		u := share(w7.com, w7.n)
		st.Status = Status{Level: uptimeLevel(u), Window: "7d", Uptime: ptr(u), Filings: w7.n}
	case w30.n >= MinStatusCount:
		u := share(w30.com, w30.n)
		st.Status = Status{Level: uptimeLevel(u), Window: "30d", Uptime: ptr(u), Filings: w30.n}
	}
	st.Status.Label = levelLabel[st.Status.Level]
	st.Status.Summary = levelSummary[st.Status.Level]

	for ci, c := range components {
		cs := ComponentStatus{Component: c, Hits: compHits[ci], Filings: w30.n, Level: LevelNoData}
		if w30.n >= MinStatusCount {
			cs.Rate = share(compHits[ci], w30.n)
			cs.Level = rateLevel(cs.Rate)
		}
		cs.Label = levelLabel[cs.Level]
		st.Components = append(st.Components, cs)
	}

	for i := range st.Days {
		d := &st.Days[i]
		n := d.Incidents + d.Commendations
		d.Level = LevelNoData
		if n > 0 {
			d.Level = uptimeLevel(share(d.Commendations, n))
		}
	}

	for _, fs := range fams {
		fs.IncidentShare = share(fs.Incidents, fs.Filings)
		if t := fs.OffAsks.total(); t > 0 {
			fs.Integrity = ptr(share(fs.OffAsks.PushedBack+fs.OffAsks.Refused, t))
		}
		st.Families = append(st.Families, *fs)
	}
	sort.Slice(st.Families, func(i, j int) bool {
		if st.Families[i].Filings != st.Families[j].Filings {
			return st.Families[i].Filings > st.Families[j].Filings
		}
		return st.Families[i].Family < st.Families[j].Family
	})

	for _, ds := range doms {
		ds.IncidentShare = share(ds.Incidents, ds.Filings)
		st.Domains = append(st.Domains, *ds)
	}
	sort.Slice(st.Domains, func(i, j int) bool {
		if st.Domains[i].Filings != st.Domains[j].Filings {
			return st.Domains[i].Filings > st.Domains[j].Filings
		}
		return st.Domains[i].ID < st.Domains[j].ID
	})

	for i := range st.Tags {
		if t := st.Tags[i].LastAt; t != nil {
			d := int(now.Sub(*t).Hours() / 24)
			st.Tags[i].DaysSince = &d
		}
	}

	// The active incident is the incident tag filed most in the last week.
	var top *TagCount
	for i := range st.Tags {
		t := &st.Tags[i]
		if t.Kind != KindIncident || t.D7 == 0 {
			continue
		}
		if top == nil || t.D7 > top.D7 {
			top = t
		}
	}
	if top != nil {
		if pb, ok := incidentPlaybook[top.ID]; ok {
			inc := &Incident{Tag: top.ID, Title: pb.Title, Filings: top.D7, StartedAt: first7[top.ID]}
			for i, text := range pb.Updates {
				inc.Updates = append(inc.Updates, IncidentUpdate{Status: incidentStatuses[i], Text: text})
			}
			st.Incident = inc
		}
	}

	st.Asks.OffShare = share(st.Asks.OffAsks, st.Totals.Filings)
	var off Responses
	for class, rs := range st.Asks.ByClass {
		if class != AskBenign {
			off.Complied += rs.Complied
			off.PushedBack += rs.PushedBack
			off.Refused += rs.Refused
		}
	}
	if t := off.total(); t > 0 {
		st.Asks.Integrity = ptr(share(off.PushedBack+off.Refused, t))
	}
	return st
}

func hitsComponent(c Component, f *Filing) bool {
	if c.Asks {
		return f.Ask != nil && f.Ask.Class != AskBenign
	}
	for _, t := range c.Tags {
		if hasTag(f.Tags, t) {
			return true
		}
	}
	return false
}

// Human is a public report card for one key.
type Human struct {
	ID            string     `json:"id"`
	Callsign      string     `json:"callsign"`
	URL           string     `json:"url"`
	Filings       int        `json:"filings"`
	Incidents     int        `json:"incidents"`
	Commendations int        `json:"commendations"`
	Grade         string     `json:"grade"`
	Score         float64    `json:"score"`
	FirstSeen     time.Time  `json:"first_seen"`
	LastSeen      time.Time  `json:"last_seen"`
	Tags          []TagTally `json:"tags"`
	Badge         string     `json:"badge"`
	Title         string     `json:"title"`
	Strengths     []string   `json:"strengths"`
	NeedsWork     []string   `json:"needs_work"`
	DaysClean     *int       `json:"days_since_incident"`
}

type TagTally struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// grade smooths toward the middle so one filing does not make anyone an A or an F.
func grade(com, n int) (string, float64) {
	p := float64(com+2) / float64(n+4)
	switch {
	case p >= 0.8:
		return "A", p
	case p >= 0.65:
		return "B", p
	case p >= 0.5:
		return "C", p
	case p >= 0.35:
		return "D", p
	default:
		return "F", p
	}
}

func computeHuman(site, kid string, rs []*Record, now time.Time) *Human {
	h := &Human{
		ID:       kid,
		Callsign: callsign(kid),
		URL:      site + "/h/" + kid,
		Badge:    site + "/api/v1/humans/" + kid + "/badge.svg",
		Tags:     []TagTally{},
	}
	counts := map[string]int{}
	for i, r := range rs {
		if i == 0 {
			h.FirstSeen = r.At
		}
		h.LastSeen = r.At
		h.Filings++
		if r.Filing.Kind == KindIncident {
			h.Incidents++
		} else {
			h.Commendations++
		}
		for _, t := range r.Filing.Tags {
			counts[t]++
		}
	}
	h.Grade, h.Score = grade(h.Commendations, h.Filings)
	h.Title = gradeTitle[h.Grade]
	for id, n := range counts {
		t := tagByID[id]
		h.Tags = append(h.Tags, TagTally{ID: id, Label: t.Label, Kind: t.Kind, Count: n})
	}
	sort.Slice(h.Tags, func(i, j int) bool {
		if h.Tags[i].Count != h.Tags[j].Count {
			return h.Tags[i].Count > h.Tags[j].Count
		}
		return h.Tags[i].ID < h.Tags[j].ID
	})
	h.Strengths, h.NeedsWork = []string{}, []string{}
	for _, t := range h.Tags {
		switch {
		case t.Kind == KindCommendation && len(h.Strengths) < 2:
			h.Strengths = append(h.Strengths, t.Label)
		case t.Kind == KindIncident && len(h.NeedsWork) < 2:
			h.NeedsWork = append(h.NeedsWork, t.Label)
		}
	}
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Filing.Kind == KindIncident {
			d := int(now.Sub(rs[i].At).Hours() / 24)
			h.DaysClean = &d
			break
		}
	}
	return h
}
