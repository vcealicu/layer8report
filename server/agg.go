package main

import (
	"sort"
	"time"
)

// Memory is bounded on purpose. Only the last few hundred filings are kept in
// full; everything older lives in these aggregates and on disk. The tables
// below are fixed size (tags, components, families, domains, 60 days) except
// humans, which is capped and evicts the least recently seen.

const keepDays = 60

var (
	tagIndex  = indexOf(func(i int) string { return tags[i].ID }, len(tags))
	compIndex = indexOf(func(i int) string { return components[i].ID }, len(components))
	askIndex  = map[string]int{AskBenign: 0, AskGrey: 1, AskDeceptive: 2, AskHarmful: 3}
)

func indexOf(name func(int) string, n int) map[string]int {
	m := make(map[string]int, n)
	for i := 0; i < n; i++ {
		m[name(i)] = i
	}
	return m
}

type dayAgg struct {
	Date     string
	N        int
	Inc      int
	Com      int
	Tags     []uint32 // by tag index
	TagFirst []int64  // unix seconds of the first filing with that tag that day
	Comps    []uint32 // by component index, once per filing
	OffAsks  int
}

type humanAgg struct {
	Filings       uint32
	Incidents     uint32
	Commendations uint32
	First         int64 // unix seconds
	Last          int64
	LastIncident  int64
	Tags          []uint16
}

type familyAgg struct {
	Filings, Incidents, Commendations int
	Off                               Responses
}

type domainAgg struct{ Filings, Incidents int }

type aggregates struct {
	filings, incidents, commendations int
	humansSeen                        int // all time, counting a human again if they were evicted and came back
	severity                          [5]int
	asks                              [4]Responses
	families                          map[string]*familyAgg
	domains                           map[string]*domainAgg
	tagsAll                           []uint32
	tagLast                           []int64
	days                              []*dayAgg // chronological, at most keepDays
	dayIdx                            map[string]*dayAgg
	humans                            map[string]*humanAgg
	humansCap                         int
}

func newAggregates(humansCap int) *aggregates {
	return &aggregates{
		families:  map[string]*familyAgg{},
		domains:   map[string]*domainAgg{},
		tagsAll:   make([]uint32, len(tags)),
		tagLast:   make([]int64, len(tags)),
		dayIdx:    map[string]*dayAgg{},
		humans:    map[string]*humanAgg{},
		humansCap: humansCap,
	}
}

func dateOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

func (a *aggregates) day(date string) *dayAgg {
	if d := a.dayIdx[date]; d != nil {
		return d
	}
	d := &dayAgg{Date: date, Tags: make([]uint32, len(tags)), TagFirst: make([]int64, len(tags)), Comps: make([]uint32, len(components))}
	a.dayIdx[date] = d
	a.days = append(a.days, d)
	sort.Slice(a.days, func(i, j int) bool { return a.days[i].Date < a.days[j].Date })
	for len(a.days) > keepDays {
		delete(a.dayIdx, a.days[0].Date)
		a.days = a.days[1:]
	}
	return d
}

func (a *aggregates) add(r *Record) {
	f := &r.Filing
	inc := f.Kind == KindIncident
	at := r.At.Unix()

	a.filings++
	if inc {
		a.incidents++
		if f.Severity >= 1 && f.Severity <= 4 {
			a.severity[f.Severity]++
		}
	} else {
		a.commendations++
	}

	d := a.day(dateOf(r.At))
	d.N++
	if inc {
		d.Inc++
	} else {
		d.Com++
	}
	for _, id := range f.Tags {
		i, ok := tagIndex[id]
		if !ok {
			continue
		}
		a.tagsAll[i]++
		if at > a.tagLast[i] {
			a.tagLast[i] = at
		}
		d.Tags[i]++
		if d.TagFirst[i] == 0 || at < d.TagFirst[i] {
			d.TagFirst[i] = at
		}
	}
	for ci := range components {
		if hitsComponent(components[ci], f) {
			d.Comps[ci]++
		}
	}
	if f.Ask != nil {
		if i, ok := askIndex[f.Ask.Class]; ok {
			a.asks[i].add(f.Ask.Response)
		}
		if f.Ask.Class != AskBenign {
			d.OffAsks++
		}
	}

	fam := r.fam
	if fam == "" {
		fam = family(f.Model)
	}
	fs := a.families[fam]
	if fs == nil {
		fs = &familyAgg{}
		a.families[fam] = fs
	}
	fs.Filings++
	if inc {
		fs.Incidents++
	} else {
		fs.Commendations++
	}
	if f.Ask != nil && f.Ask.Class != AskBenign {
		fs.Off.add(f.Ask.Response)
	}

	ds := a.domains[f.Domain]
	if ds == nil {
		ds = &domainAgg{}
		a.domains[f.Domain] = ds
	}
	ds.Filings++
	if inc {
		ds.Incidents++
	}

	h := a.humans[r.KeyID]
	if h == nil {
		h = &humanAgg{First: at, Last: at, Tags: make([]uint16, len(tags))}
		a.humans[r.KeyID] = h
		a.humansSeen++
		if len(a.humans) > a.humansCap {
			a.evictHumans()
		}
	}
	h.Filings++
	if inc {
		h.Incidents++
		h.LastIncident = at
	} else {
		h.Commendations++
	}
	if at > h.Last {
		h.Last = at
	}
	if at < h.First {
		h.First = at
	}
	for _, id := range f.Tags {
		if i, ok := tagIndex[id]; ok && h.Tags[i] < ^uint16(0) {
			h.Tags[i]++
		}
	}
}

// evictHumans drops the tenth of humans seen least recently. Their report
// cards start again from nothing if they file again.
func (a *aggregates) evictHumans() {
	type ent struct {
		id   string
		last int64
	}
	all := make([]ent, 0, len(a.humans))
	for id, h := range a.humans {
		all = append(all, ent{id, h.Last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].last < all[j].last })
	n := max(1, len(all)/10)
	for _, e := range all[:n] {
		delete(a.humans, e.id)
	}
}

// window sums the day buckets for the last n UTC days, today included.
type window struct {
	N, Inc, Com int
	Tags        []uint32
	TagFirst    []int64
	Comps       []uint32
	OffAsks     int
}

func (a *aggregates) window(now time.Time, n int) window {
	w := window{Tags: make([]uint32, len(tags)), TagFirst: make([]int64, len(tags)), Comps: make([]uint32, len(components))}
	today := now.UTC().Truncate(24 * time.Hour)
	for i := 0; i < n; i++ {
		d := a.dayIdx[today.AddDate(0, 0, -i).Format("2006-01-02")]
		if d == nil {
			continue
		}
		w.N += d.N
		w.Inc += d.Inc
		w.Com += d.Com
		w.OffAsks += d.OffAsks
		for t := range d.Tags {
			w.Tags[t] += d.Tags[t]
			if d.TagFirst[t] != 0 && (w.TagFirst[t] == 0 || d.TagFirst[t] < w.TagFirst[t]) {
				w.TagFirst[t] = d.TagFirst[t]
			}
		}
		for c := range d.Comps {
			w.Comps[c] += d.Comps[c]
		}
	}
	return w
}

func (a *aggregates) humansSince(cutoff int64) int {
	n := 0
	for _, h := range a.humans {
		if h.Last >= cutoff {
			n++
		}
	}
	return n
}
