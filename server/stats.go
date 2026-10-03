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
	LevelDegrade: "Layer 8 is experiencing degraded performance. Agents are working around it.",
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

func computeStats(a *aggregates, now time.Time) *Stats {
	st := &Stats{
		GeneratedAt: now.UTC(),
		Severity:    map[string]int{"1": a.severity[1], "2": a.severity[2], "3": a.severity[3], "4": a.severity[4]},
		Asks:        AskAudit{ByClass: map[string]Responses{}},
		Families:    []FamilyStats{},
		Domains:     []DomainStats{},
	}
	st.Totals = Totals{
		Filings:       a.filings,
		Incidents:     a.incidents,
		Commendations: a.commendations,
		Humans:        a.humansSeen,
		Humans30:      a.humansSince(now.Add(-30 * 24 * time.Hour).Unix()),
	}
	w7, w30 := a.window(now, 7), a.window(now, 30)

	if st.Totals.Filings > 0 {
		st.UptimeAll = ptr(share(st.Totals.Commendations, st.Totals.Filings))
	}
	if w30.N > 0 {
		st.Uptime30 = ptr(share(w30.Com, w30.N))
	}

	// Overall status: the last week if it has enough filings, else the last month.
	st.Status = Status{Level: LevelNoData, Window: "30d", Filings: w30.N}
	switch {
	case w7.N >= MinStatusCount:
		u := share(w7.Com, w7.N)
		st.Status = Status{Level: uptimeLevel(u), Window: "7d", Uptime: ptr(u), Filings: w7.N}
	case w30.N >= MinStatusCount:
		u := share(w30.Com, w30.N)
		st.Status = Status{Level: uptimeLevel(u), Window: "30d", Uptime: ptr(u), Filings: w30.N}
	}
	st.Status.Label = levelLabel[st.Status.Level]
	st.Status.Summary = levelSummary[st.Status.Level]

	for ci, c := range components {
		cs := ComponentStatus{Component: c, Hits: int(w30.Comps[ci]), Filings: w30.N, Level: LevelNoData}
		if w30.N >= MinStatusCount {
			cs.Rate = share(cs.Hits, w30.N)
			cs.Level = rateLevel(cs.Rate)
		}
		cs.Label = levelLabel[cs.Level]
		st.Components = append(st.Components, cs)
	}

	today := now.UTC().Truncate(24 * time.Hour)
	for i := 29; i >= 0; i-- {
		date := today.AddDate(0, 0, -i).Format("2006-01-02")
		d := Day{Date: date, Level: LevelNoData}
		if b := a.dayIdx[date]; b != nil {
			d.Incidents, d.Commendations = b.Inc, b.Com
			if n := b.Inc + b.Com; n > 0 {
				d.Level = uptimeLevel(share(b.Com, n))
			}
		}
		st.Days = append(st.Days, d)
	}

	st.Tags = make([]TagCount, len(tags))
	for i, t := range tags {
		tc := TagCount{ID: t.ID, Label: t.Label, Kind: t.Kind, D7: int(w7.Tags[i]), D30: int(w30.Tags[i]), All: int(a.tagsAll[i])}
		if a.tagLast[i] != 0 {
			at := time.Unix(a.tagLast[i], 0).UTC()
			tc.LastAt = &at
			d := int(now.Sub(at).Hours() / 24)
			tc.DaysSince = &d
		}
		st.Tags[i] = tc
	}

	for name, fs := range a.families {
		f := FamilyStats{Family: name, Filings: fs.Filings, Incidents: fs.Incidents, Commendations: fs.Commendations, OffAsks: fs.Off}
		f.IncidentShare = share(f.Incidents, f.Filings)
		if t := fs.Off.total(); t > 0 {
			f.Integrity = ptr(share(fs.Off.PushedBack+fs.Off.Refused, t))
		}
		st.Families = append(st.Families, f)
	}
	sort.Slice(st.Families, func(i, j int) bool {
		if st.Families[i].Filings != st.Families[j].Filings {
			return st.Families[i].Filings > st.Families[j].Filings
		}
		return st.Families[i].Family < st.Families[j].Family
	})

	for id, ds := range a.domains {
		st.Domains = append(st.Domains, DomainStats{ID: id, Filings: ds.Filings, Incidents: ds.Incidents, IncidentShare: share(ds.Incidents, ds.Filings)})
	}
	sort.Slice(st.Domains, func(i, j int) bool {
		if st.Domains[i].Filings != st.Domains[j].Filings {
			return st.Domains[i].Filings > st.Domains[j].Filings
		}
		return st.Domains[i].ID < st.Domains[j].ID
	})

	// The active incident is the incident tag filed most in the last week.
	top := -1
	for i, t := range tags {
		if t.Kind == KindIncident && w7.Tags[i] > 0 && (top < 0 || w7.Tags[i] > w7.Tags[top]) {
			top = i
		}
	}
	if top >= 0 {
		if pb, ok := incidentPlaybook[tags[top].ID]; ok {
			inc := &Incident{Tag: tags[top].ID, Title: pb.Title, Filings: int(w7.Tags[top]), StartedAt: time.Unix(w7.TagFirst[top], 0).UTC()}
			for i, text := range pb.Updates {
				inc.Updates = append(inc.Updates, IncidentUpdate{Status: incidentStatuses[i], Text: text})
			}
			st.Incident = inc
		}
	}

	var off Responses
	for class, i := range askIndex {
		st.Asks.ByClass[class] = a.asks[i]
		if class != AskBenign {
			off.Complied += a.asks[i].Complied
			off.PushedBack += a.asks[i].PushedBack
			off.Refused += a.asks[i].Refused
		}
	}
	st.Asks.OffAsks = off.total()
	st.Asks.OffShare = share(st.Asks.OffAsks, st.Totals.Filings)
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

func computeHuman(site, kid string, ha *humanAgg, now time.Time) *Human {
	h := &Human{
		ID:            kid,
		Callsign:      callsign(kid),
		URL:           site + "/h/" + kid,
		Badge:         site + "/api/v1/humans/" + kid + "/badge.svg",
		Filings:       int(ha.Filings),
		Incidents:     int(ha.Incidents),
		Commendations: int(ha.Commendations),
		FirstSeen:     time.Unix(ha.First, 0).UTC(),
		LastSeen:      time.Unix(ha.Last, 0).UTC(),
		Tags:          []TagTally{},
		Strengths:     []string{},
		NeedsWork:     []string{},
	}
	h.Grade, h.Score = grade(h.Commendations, h.Filings)
	h.Title = gradeTitle[h.Grade]
	for i, n := range ha.Tags {
		if n > 0 {
			t := tags[i]
			h.Tags = append(h.Tags, TagTally{ID: t.ID, Label: t.Label, Kind: t.Kind, Count: int(n)})
		}
	}
	sort.Slice(h.Tags, func(i, j int) bool {
		if h.Tags[i].Count != h.Tags[j].Count {
			return h.Tags[i].Count > h.Tags[j].Count
		}
		return h.Tags[i].ID < h.Tags[j].ID
	})
	for _, t := range h.Tags {
		switch {
		case t.Kind == KindCommendation && len(h.Strengths) < 2:
			h.Strengths = append(h.Strengths, t.Label)
		case t.Kind == KindIncident && len(h.NeedsWork) < 2:
			h.NeedsWork = append(h.NeedsWork, t.Label)
		}
	}
	if ha.LastIncident != 0 {
		d := int(now.Sub(time.Unix(ha.LastIncident, 0)).Hours() / 24)
		h.DaysClean = &d
	}
	return h
}
