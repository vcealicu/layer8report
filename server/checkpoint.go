package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// A checkpoint is everything the store derives from the files, written to
// dir/checkpoint.json with the position in the files it covers. Opening the
// store from one reads a few megabytes at most, whatever the history, then
// replays only the filings after the position.
//
// Tag, component and ask counts are keyed by id, not by position, so a
// taxonomy change (a tag added, removed or reordered) cannot misread an old
// checkpoint. Counts for a tag that no longer exists are dropped.
//
// Nothing in it identifies anyone: the aggregates, the recent filings (which
// are public), and the limiter's per-key and global counts. IP addresses are
// never part of it.

const checkpointVersion = 1

type checkpoint struct {
	V       int             `json:"v"`
	Written time.Time       `json:"written"`
	Month   string          `json:"month"`  // the file the position is in
	Offset  int64           `json:"offset"` // bytes of that file already folded in
	LastSeq int64           `json:"last_seq"`
	Ring    []*Record       `json:"recent"`
	Agg     aggSnapshot     `json:"aggregates"`
	Extra   json.RawMessage `json:"extra,omitempty"` // the caller's own state, see storeHooks
}

type aggSnapshot struct {
	Filings       int                   `json:"filings"`
	Incidents     int                   `json:"incidents"`
	Commendations int                   `json:"commendations"`
	HumansSeen    int                   `json:"humans_seen"`
	Severity      [5]int                `json:"severity"`
	Asks          map[string]Responses  `json:"asks"`
	Families      map[string]*familyAgg `json:"families"`
	Domains       map[string]*domainAgg `json:"domains"`
	Tags          map[string]uint32     `json:"tags"`
	TagLast       map[string]int64      `json:"tag_last"`
	Days          []daySnapshot         `json:"days"`
	Humans        map[string]humanSnap  `json:"humans"`
}

type daySnapshot struct {
	Date     string            `json:"date"`
	N        int               `json:"n"`
	Inc      int               `json:"inc"`
	Com      int               `json:"com"`
	OffAsks  int               `json:"off_asks"`
	Tags     map[string]uint32 `json:"tags,omitempty"`
	TagFirst map[string]int64  `json:"tag_first,omitempty"`
	Comps    map[string]uint32 `json:"comps,omitempty"`
}

type humanSnap struct {
	Filings       uint32            `json:"filings"`
	Incidents     uint32            `json:"incidents"`
	Commendations uint32            `json:"commendations"`
	First         int64             `json:"first"`
	Last          int64             `json:"last"`
	LastIncident  int64             `json:"last_incident"`
	Tags          map[string]uint16 `json:"tags,omitempty"`
}

var askNames = [4]string{AskBenign, AskGrey, AskDeceptive, AskHarmful}

func (s *store) checkpointPath() string { return filepath.Join(s.dir, "checkpoint.json") }

// byID turns a slice indexed by tag or component position into a map by id,
// leaving out zeros.
func byID[T uint16 | uint32 | int64](vals []T, id func(int) string) map[string]T {
	m := map[string]T{}
	for i, v := range vals {
		if v != 0 {
			m[id(i)] = v
		}
	}
	return m
}

// fromID is the reverse, ignoring ids that are no longer in the taxonomy.
func fromID[T uint16 | uint32 | int64](m map[string]T, index map[string]int, n int) []T {
	out := make([]T, n)
	for id, v := range m {
		if i, ok := index[id]; ok {
			out[i] = v
		}
	}
	return out
}

func tagID(i int) string  { return tags[i].ID }
func compID(i int) string { return components[i].ID }

func (a *aggregates) snapshot() aggSnapshot {
	out := aggSnapshot{
		Filings: a.filings, Incidents: a.incidents, Commendations: a.commendations, HumansSeen: a.humansSeen,
		Severity: a.severity, Asks: map[string]Responses{}, Families: a.families, Domains: a.domains,
		Tags: byID(a.tagsAll, tagID), TagLast: byID(a.tagLast, tagID), Humans: make(map[string]humanSnap, len(a.humans)),
	}
	for i, r := range a.asks {
		out.Asks[askNames[i]] = r
	}
	for _, d := range a.days {
		out.Days = append(out.Days, daySnapshot{
			Date: d.Date, N: d.N, Inc: d.Inc, Com: d.Com, OffAsks: d.OffAsks,
			Tags: byID(d.Tags, tagID), TagFirst: byID(d.TagFirst, tagID), Comps: byID(d.Comps, compID),
		})
	}
	for id, h := range a.humans {
		out.Humans[id] = humanSnap{
			Filings: h.Filings, Incidents: h.Incidents, Commendations: h.Commendations,
			First: h.First, Last: h.Last, LastIncident: h.LastIncident, Tags: byID(h.Tags, tagID),
		}
	}
	return out
}

func (a *aggregates) load(in aggSnapshot) {
	a.filings, a.incidents, a.commendations, a.humansSeen = in.Filings, in.Incidents, in.Commendations, in.HumansSeen
	a.severity = in.Severity
	for name, r := range in.Asks {
		if i, ok := askIndex[name]; ok {
			a.asks[i] = r
		}
	}
	if in.Families != nil {
		a.families = in.Families
	}
	if in.Domains != nil {
		a.domains = in.Domains
	}
	a.tagsAll = fromID(in.Tags, tagIndex, len(tags))
	a.tagLast = fromID(in.TagLast, tagIndex, len(tags))
	for _, d := range in.Days {
		day := a.day(d.Date) // keeps the window at keepDays and sorted
		day.N, day.Inc, day.Com, day.OffAsks = d.N, d.Inc, d.Com, d.OffAsks
		day.Tags = fromID(d.Tags, tagIndex, len(tags))
		day.TagFirst = fromID(d.TagFirst, tagIndex, len(tags))
		day.Comps = fromID(d.Comps, compIndex, len(components))
	}
	for id, h := range in.Humans {
		a.humans[id] = &humanAgg{
			Filings: h.Filings, Incidents: h.Incidents, Commendations: h.Commendations,
			First: h.First, Last: h.Last, LastIncident: h.LastIncident, Tags: fromID(h.Tags, tagIndex, len(tags)),
		}
	}
	for len(a.humans) > a.humansCap {
		a.evictHumans() // -humans was lowered since the checkpoint
	}
}

// readCheckpoint returns the checkpoint if it is there, readable and agrees
// with the files: its month must exist and hold at least Offset bytes. Anything
// else means a full replay, which is slow but always right.
func (s *store) readCheckpoint(months []string) (*checkpoint, bool) {
	b, err := os.ReadFile(s.checkpointPath())
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("store: checkpoint unreadable, replaying every file: %v", err)
		}
		return nil, false
	}
	ck := new(checkpoint)
	if err := json.Unmarshal(b, ck); err != nil || ck.V != checkpointVersion {
		log.Printf("store: checkpoint is damaged or from another version, replaying every file")
		return nil, false
	}
	if ck.Month == "" && ck.LastSeq == 0 {
		return ck, true // written before anything was filed
	}
	found := false
	for _, m := range months {
		if m == ck.Month {
			found = true
		}
	}
	if fi, err := os.Stat(s.fileFor(ck.Month)); !found || err != nil || fi.Size() < ck.Offset {
		log.Printf("store: checkpoint points past the files (%s at %d), replaying every file", ck.Month, ck.Offset)
		return nil, false
	}
	return ck, true
}

func (s *store) restore(ck *checkpoint) {
	s.agg.load(ck.Agg)
	for _, r := range ck.Ring {
		r.fam = family(r.Filing.Model)
	}
	if len(ck.Ring) > s.keep {
		ck.Ring = ck.Ring[len(ck.Ring)-s.keep:] // -keep was lowered since the checkpoint
	}
	s.ring = ck.Ring
	s.lastSeq = ck.LastSeq
	s.ckptSeq = ck.LastSeq
}

// changedSinceCheckpoint reports whether anything was filed since the last one.
func (s *store) changedSinceCheckpoint() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastSeq != s.ckptSeq
}

// saveCheckpoint writes the checkpoint atomically: a temporary file, fsync,
// rename, fsync of the directory. extra is saved alongside for the caller.
// The caller must stop filings while it runs (the server holds wmu), so the
// store and the caller's state describe the same moment.
func (s *store) saveCheckpoint(extra json.RawMessage, now time.Time) error {
	s.mu.RLock()
	ck := checkpoint{
		V: checkpointVersion, Written: now.UTC(), Month: s.month, Offset: s.size,
		LastSeq: s.lastSeq, Ring: s.ring, Agg: s.agg.snapshot(), Extra: extra,
	}
	b, err := json.Marshal(ck)
	seq := s.lastSeq
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := s.checkpointPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if err := os.Rename(tmp, s.checkpointPath()); err != nil {
		return err
	}
	if d, err := os.Open(s.dir); err == nil {
		d.Sync()
		d.Close()
	}
	s.mu.Lock()
	s.ckptSeq = seq
	s.mu.Unlock()
	return nil
}
