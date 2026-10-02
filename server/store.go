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

// store is an append-only JSONL file with everything indexed in memory.
// It is fine into the millions of records; past that, move it to Postgres.
type store struct {
	mu    sync.RWMutex
	path  string
	f     *os.File
	recs  []*Record
	byKey map[string][]*Record
	stats *Stats
	dirty bool
	built time.Time
	size  int64 // bytes of complete lines on disk
}

func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	s := &store{path: path, byKey: map[string][]*Record{}, dirty: true}
	if err := s.repair(); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
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

func (s *store) load() error {
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
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			// A torn last line after a crash is expected. Anything else is logged and skipped.
			log.Printf("store: skipping line %d: %v", line, err)
			continue
		}
		s.index(&r)
	}
	return sc.Err()
}

func (s *store) index(r *Record) {
	r.fam = family(r.Filing.Model)
	s.recs = append(s.recs, r)
	s.byKey[r.KeyID] = append(s.byKey[r.KeyID], r)
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
	return len(s.byKey[kid]) > 0
}

func (s *store) nextSeq() int64 {
	if len(s.recs) == 0 {
		return 1
	}
	return s.recs[len(s.recs)-1].Seq + 1
}

// append assigns a sequence number, writes, syncs, then indexes.
func (s *store) append(r *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Seq = s.nextSeq()
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
	s.index(r)
	return nil
}

func (s *store) get(seq int64) *Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Sequence numbers are dense from 1, so this is usually a direct hit.
	if i := int(seq - 1); i >= 0 && i < len(s.recs) && s.recs[i].Seq == seq {
		return s.recs[i]
	}
	i := sort.Search(len(s.recs), func(i int) bool { return s.recs[i].Seq >= seq })
	if i < len(s.recs) && s.recs[i].Seq == seq {
		return s.recs[i]
	}
	return nil
}

type feedQuery struct {
	Before int64
	Limit  int
	Kind   string
	Tag    string
	Human  string
	Family string
}

func (s *store) feed(q feedQuery) (out []*Record, more bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.recs
	if q.Human != "" {
		src = s.byKey[q.Human]
	}
	for i := len(src) - 1; i >= 0; i-- {
		r := src[i]
		if q.Before > 0 && r.Seq >= q.Before {
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
		if len(out) == q.Limit {
			return out, true
		}
		out = append(out, r)
	}
	return out, false
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
	// Someone else may have rebuilt while we waited for the lock.
	if s.stats != nil && now.Sub(s.built) < 5*time.Second {
		return s.stats
	}
	s.stats = computeStats(s.recs, s.byKey, now)
	s.dirty = false
	s.built = now
	return s.stats
}

func (s *store) human(kid string) []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rs := s.byKey[kid]
	out := make([]*Record, len(rs))
	copy(out, rs)
	return out
}

func refFor(r *Record) string {
	p := "INC"
	if r.Filing.Kind == KindCommendation {
		p = "KUDOS"
	}
	return fmt.Sprintf("%s-%06d", p, r.Seq)
}

func idFor(seq int64) string { return fmt.Sprintf("%06d", seq) }
