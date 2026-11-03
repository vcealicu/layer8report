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
	"strings"
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

// store keeps filings in append-only JSONL files, one per UTC month, under
// dir/filings/YYYY-MM.jsonl, with a bounded view in memory: the last `keep`
// filings in full, plus aggregates for everything. Memory use does not grow
// with the number of filings.
//
// Startup does not grow with them either. dir/checkpoint.json holds the
// aggregates, the recent filings and the position in the files they cover, so
// opening the store reads the checkpoint and replays only what was filed after
// it. Without a usable checkpoint it replays every file, once.
type store struct {
	mu       sync.RWMutex
	dir      string
	f        *os.File
	month    string // the file f appends to, "2006-01"
	size     int64  // bytes of complete lines in that file
	keep     int
	ring     []*Record // chronological, at most keep
	lastSeq  int64
	archived int64 // filings no longer held in full
	agg      *aggregates
	stats    *Stats
	built    time.Time
	dirty    bool
	ckptSeq  int64 // lastSeq covered by the checkpoint on disk
	opened   openReport
}

// storeHooks lets the caller rebuild its own state while the store opens.
// Restore gets the extra state saved with the checkpoint (the limiter's
// key-based counts), Replay every filing read from the files after it.
type storeHooks struct {
	Restore func(extra json.RawMessage)
	Replay  func(*Record)
}

// openReport says how the store came up, for the startup log and the tests.
type openReport struct {
	FromCheckpoint bool
	Replayed       int // filings folded in from the files
	Read           int // lines read from the files, Replayed plus any the checkpoint already had
	Migrated       int // filings moved from the old single file
	Files          int
}

func monthOf(t time.Time) string { return t.UTC().Format("2006-01") }

func (s *store) filesDir() string            { return filepath.Join(s.dir, "filings") }
func (s *store) fileFor(month string) string { return filepath.Join(s.filesDir(), month+".jsonl") }

// months lists the month files on disk, oldest first.
func (s *store) months() ([]string, error) {
	ents, err := os.ReadDir(s.filesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		m := strings.TrimSuffix(name, ".jsonl")
		if _, err := time.Parse("2006-01", m); err == nil {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}

func openStore(dir string, keep, humansCap int, hooks storeHooks) (*store, error) {
	s := &store{dir: dir, keep: max(keep, 1), agg: newAggregates(max(humansCap, 1)), dirty: true}
	if err := os.MkdirAll(s.filesDir(), 0o750); err != nil {
		return nil, err
	}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	months, err := s.months()
	if err != nil {
		return nil, err
	}
	s.opened.Files = len(months)
	if len(months) > 0 {
		s.month = months[len(months)-1]
		if s.size, err = repair(s.fileFor(s.month)); err != nil {
			return nil, err
		}
	}

	from, offset := "", int64(0)
	if ck, ok := s.readCheckpoint(months); ok {
		s.restore(ck)
		if hooks.Restore != nil && len(ck.Extra) > 0 {
			hooks.Restore(ck.Extra)
		}
		from, offset = ck.Month, ck.Offset
		s.opened.FromCheckpoint = true
	}
	for _, m := range months {
		if m < from {
			continue
		}
		start := int64(0)
		if m == from {
			start = offset
		}
		if err := s.replay(m, start, hooks.Replay); err != nil {
			return nil, err
		}
	}
	s.archived = int64(s.agg.filings - len(s.ring))
	if s.month != "" {
		if s.f, err = os.OpenFile(s.fileFor(s.month), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// migrate splits the single reports.jsonl that earlier versions wrote into
// month files, once, and keeps the original as reports.jsonl.migrated.
func (s *store) migrate() error {
	old := filepath.Join(s.dir, "reports.jsonl")
	if _, err := os.Stat(old); err != nil {
		return nil
	}
	if months, err := s.months(); err != nil || len(months) > 0 {
		return err
	}
	if _, err := repair(old); err != nil {
		return err
	}
	in, err := os.Open(old)
	if err != nil {
		return err
	}
	defer in.Close()
	out := map[string]*os.File{}
	defer func() {
		for _, f := range out {
			f.Close()
		}
	}()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r struct {
			At time.Time `json:"at"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			continue // load skips it too
		}
		m := monthOf(r.At)
		f := out[m]
		if f == nil {
			if f, err = os.OpenFile(s.fileFor(m), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640); err != nil {
				return err
			}
			out[m] = f
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
		s.opened.Migrated++
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for _, f := range out {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	log.Printf("store: moved %d filings from %s into month files", s.opened.Migrated, old)
	return os.Rename(old, old+".migrated")
}

// repair cuts a torn last line left by a crash, so the next append starts on
// a fresh line instead of being glued to the fragment. Only the tail is read.
// It returns the size of the complete lines.
func repair(path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := fi.Size()
	tail := min(size, 1<<20) // a line is a few KB at most
	buf := make([]byte, tail)
	if _, err := f.ReadAt(buf, size-tail); err != nil && err != io.EOF {
		return 0, err
	}
	keep := size
	if tail > 0 && buf[tail-1] != '\n' {
		keep = size - tail + int64(bytes.LastIndexByte(buf, '\n')+1)
		log.Printf("store: dropping %d bytes of torn last line in %s", size-keep, filepath.Base(path))
		if err := f.Truncate(keep); err != nil {
			return 0, err
		}
	}
	return keep, nil
}

// replay reads one month file from byte offset start.
func (s *store) replay(month string, start int64, onLoad func(*Record)) error {
	f, err := os.Open(s.fileFor(month))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		s.opened.Read++
		r := new(Record)
		if err := json.Unmarshal(sc.Bytes(), r); err != nil {
			log.Printf("store: skipping %s line %d after byte %d: %v", month, line, start, err)
			continue
		}
		if r.Seq <= s.lastSeq {
			continue // already in the checkpoint
		}
		s.ingest(r)
		s.opened.Replayed++
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
	if s.f == nil {
		return nil
	}
	return s.f.Close()
}

func (s *store) hasKey(kid string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.agg.humans[kid]
	return ok
}

// append assigns a sequence number, writes to this month's file, syncs, then
// ingests. A new month starts a new file.
func (s *store) append(r *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := monthOf(r.At); s.f == nil || m > s.month {
		f, err := os.OpenFile(s.fileFor(m), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if s.f != nil {
			s.f.Close()
		}
		s.f, s.month, s.size = f, m, fi.Size()
	}
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
