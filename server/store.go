package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record is one stored filing. Body holds the exact signed bytes so anyone can
// re-verify Sig against Key later. IP addresses are never part of a record.
type Record struct {
	Seq    int64     `json:"seq"`
	At     time.Time `json:"at"`
	KeyID  string    `json:"key_id"`
	Key    string    `json:"key"`
	Sig    string    `json:"sig"`
	Body   string    `json:"body"`
	Filing Filing    `json:"filing"`
	fam    string    // model family, worked out once at index time
}

// store is an append-only JSONL file on disk with a bounded view in memory:
// the last `keep` filings in full, plus aggregates for everything. Memory use
// does not grow with the number of filings.
type store struct {
	mu       sync.RWMutex
	path     string
	f        *os.File
	size     int64 // bytes of complete lines on disk
	keep     int
	ring     []*Record // chronological, at most keep
	lastSeq  int64
	archived int64 // filings no longer held in full
	agg      *aggregates
	stats    *Stats
	built    time.Time
	dirty    bool
}

// openStore replays the file, calling onLoad for every record so the caller can
// rebuild its own state without the records being kept.
func openStore(path string, keep, humansCap int, onLoad func(*Record)) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	s := &store{path: path, keep: max(keep, 1), agg: newAggregates(max(humansCap, 1)), dirty: true}
	if err := s.repair(); err != nil {
		return nil, err
	}
	if err := s.load(onLoad); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	s.f = f
	return s, nil
}

// repair cuts a torn last line left by a crash, so the next append starts on
// a fresh line instead of being glued to the fragment. Only the tail is read.
func (s *store) repair() error {
	f, err := os.OpenFile(s.path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	tail := min(size, 1<<20) // a line is a few KB at most
	buf := make([]byte, tail)
	if _, err := f.ReadAt(buf, size-tail); err != nil && err != io.EOF {
		return err
	}
	keep := size
	if tail > 0 && buf[tail-1] != '\n' {
		keep = size - tail + int64(bytes.LastIndexByte(buf, '\n')+1)
		log.Printf("store: dropping %d bytes of torn last line", size-keep)
		if err := f.Truncate(keep); err != nil {
			return err
		}
	}
	s.size = keep
	return nil
}

func (s *store) load(onLoad func(*Record)) error {
	f, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		r := new(Record)
		if err := json.Unmarshal(sc.Bytes(), r); err != nil {
			log.Printf("store: skipping line %d: %v", line, err)
			continue
		}
		s.ingest(r)
		if onLoad != nil {
			onLoad(r)
		}
	}
	return sc.Err()
}

// ingest folds a record into the aggregates and the ring.
func (s *store) ingest(r *Record) {
	r.fam = family(r.Filing.Model)
	s.agg.add(r)
	if len(s.ring) == s.keep {
		copy(s.ring, s.ring[1:])
		s.ring[len(s.ring)-1] = r
		s.archived++
	} else {
		s.ring = append(s.ring, r)
	}
	if r.Seq > s.lastSeq {
		s.lastSeq = r.Seq
	}
	s.dirty = true
}

func (s *store) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

func (s *store) hasKey(kid string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.agg.humans[kid]
	return ok
}

// append assigns a sequence number, writes, syncs, then ingests.
func (s *store) append(r *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Seq = s.lastSeq + 1
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = s.f.Write(b)
	if err == nil {
		err = s.f.Sync()
	}
	if err != nil {
		// Put the file back how it was so a partial line cannot swallow the next one.
		if terr := s.f.Truncate(s.size); terr != nil {
			log.Printf("store: truncate after failed write: %v", terr)
		}
		return err
	}
	s.size += int64(len(b))
	s.ingest(r)
	return nil
}

type lookup int

const (
	lookupFound lookup = iota
	lookupArchived
	lookupMissing
)

// get finds a filing still held in full. Older filings report archived.
func (s *store) get(seq int64) (*Record, lookup) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if seq < 1 || seq > s.lastSeq {
		return nil, lookupMissing
	}
	i := sort.Search(len(s.ring), func(i int) bool { return s.ring[i].Seq >= seq })
	if i < len(s.ring) && s.ring[i].Seq == seq {
		return s.ring[i], lookupFound
	}
	return nil, lookupArchived
}

type feedQuery struct {
	Before int64
	Limit  int
	Kind   string
	Tag    string
	Human  string
	Family string
}

type feedPage struct {
	Recs     []*Record
	More     bool
	Archived int64
	Kept     int
}

func (s *store) feed(q feedQuery) feedPage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := feedPage{Archived: s.archived, Kept: s.keep}
	for i := len(s.ring) - 1; i >= 0; i-- {
		r := s.ring[i]
		if q.Before > 0 && r.Seq >= q.Before {
			continue
		}
		if q.Human != "" && r.KeyID != q.Human {
			continue
		}
		if q.Kind != "" && r.Filing.Kind != q.Kind {
			continue
		}
		if q.Tag != "" && !hasTag(r.Filing.Tags, q.Tag) {
			continue
		}
		if q.Family != "" && r.fam != q.Family {
			continue
		}
		if len(p.Recs) == q.Limit {
			p.More = true
			return p
		}
		p.Recs = append(p.Recs, r)
	}
	return p
}

func hasTag(ts []string, t string) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

// snapshot returns cached stats, rebuilding at most every few seconds.
func (s *store) snapshot(now time.Time) *Stats {
	s.mu.RLock()
	fresh := s.stats != nil && (!s.dirty || now.Sub(s.built) < 5*time.Second) && now.Sub(s.built) < time.Minute
	st := s.stats
	s.mu.RUnlock()
	if fresh {
		return st
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stats != nil && now.Sub(s.built) < 5*time.Second {
		return s.stats
	}
	s.stats = computeStats(s.agg, now)
	s.dirty = false
	s.built = now
	return s.stats
}

// human returns a copy of the aggregate for one key and their recent filings
// still held in full, newest first.
func (s *store) human(kid string, limit int) (*humanAgg, []*Record) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.agg.humans[kid]
	if h == nil {
		return nil, nil
	}
	cp := *h
	cp.Tags = append([]uint16(nil), h.Tags...)
	var recent []*Record
	for i := len(s.ring) - 1; i >= 0 && len(recent) < limit; i-- {
		if s.ring[i].KeyID == kid {
			recent = append(recent, s.ring[i])
		}
	}
	return &cp, recent
}

type storeSizes struct {
	Filings  int64 `json:"filings"`
	Kept     int   `json:"kept_in_full"`
	Archived int64 `json:"archived"`
	Humans   int   `json:"humans_tracked"`
	Days     int   `json:"days"`
}

func (s *store) sizes() storeSizes {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return storeSizes{Filings: s.lastSeq, Kept: len(s.ring), Archived: s.archived, Humans: len(s.agg.humans), Days: len(s.agg.days)}
}

func refFor(r *Record) string {
	p := "INC"
	if r.Filing.Kind == KindCommendation {
		p = "KUDOS"
	}
	return fmt.Sprintf("%s-%06d", p, r.Seq)
}

func idFor(seq int64) string { return fmt.Sprintf("%06d", seq) }
