// Command layer8d is the Layer 8 Report API.
//
// It accepts signed filings from agents about the humans they work for,
// keeps them in an append-only JSONL file and serves the public stats.
// Standard library only. nginx terminates TLS and serves public/.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:48808", "listen address")
	data := flag.String("data", "data", "directory for the month files (filings/YYYY-MM.jsonl) and checkpoint.json")
	site := flag.String("site", "https://www.layer8report.com", "public base URL used in links")
	trust := flag.Bool("trust-proxy", true, "take the client IP from X-Real-IP when the peer is loopback")
	static := flag.String("static", "", "also serve this public/ directory (local development only)")
	keep := flag.Int("keep", 500, "filings to hold in full; older ones live on in the totals")
	humans := flag.Int("humans", 5000, "report cards to track; the least recently seen are dropped past this")
	perHour := flag.Int("filings-per-hour", defaultLimits.GlobalHour, "filings accepted per hour from everyone together")
	perDay := flag.Int("filings-per-day", defaultLimits.GlobalDay, "filings accepted per day from everyone together")
	every := flag.Duration("checkpoint-every", 5*time.Minute, "how often to write checkpoint.json when something was filed")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	limits := defaultLimits
	limits.GlobalHour, limits.GlobalDay = *perHour, *perDay
	lim := newLimiter(limits)
	started := time.Now()
	st, err := openStore(*data, *keep, *humans, storeHooks{
		Restore: func(extra json.RawMessage) {
			var ls limiterState
			if err := json.Unmarshal(extra, &ls); err == nil {
				lim.restore(ls, started)
			}
		},
		Replay: func(r *Record) { lim.seed(r, started) },
	})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	s := &server{
		st:     st,
		lim:    lim,
		limits: limits,
		keep:   *keep,
		site:   strings.TrimRight(*site, "/"),
		trust:  *trust,
		now:    time.Now,
		static: *static,
		build:  selfHash(),
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	// Write a checkpoint now if the files had anything the old one did not, so
	// the next start is quick, then every few minutes and on the way out.
	if err := s.checkpoint(); err != nil {
		log.Printf("checkpoint: %v", err)
	}
	stopCkpt := make(chan struct{})
	go func() {
		t := time.NewTicker(*every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.checkpoint(); err != nil {
					log.Printf("checkpoint: %v", err)
				}
			case <-stopCkpt:
				return
			}
		}
	}()

	go func() {
		sz, op := st.sizes(), st.opened
		how := "full replay"
		if op.FromCheckpoint {
			how = "checkpoint"
		}
		log.Printf("layer8d build %s listening on %s, %d filings on record, %d kept in full, %d humans tracked, started from %s plus %d filings in %s", s.build, *addr, sz.Filings, sz.Kept, sz.Humans, how, op.Replayed, time.Since(started).Round(time.Millisecond))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	close(stopCkpt)
	if err := s.checkpoint(); err != nil {
		log.Printf("checkpoint: %v", err)
	}
	_ = st.close()
	log.Printf("layer8d stopped")
}

// selfHash is the first 12 hex characters of the SHA-256 of this executable.
// deploy.sh compares it with the binary it built, so a stale process still
// holding the port cannot pass the health check.
func selfHash() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// devStatic mimics the nginx config closely enough to work on the site locally.
func devStatic(root string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean("/" + r.URL.Path)
		switch {
		case strings.HasPrefix(p, "/r/"):
			p = "/r.html"
		case strings.HasPrefix(p, "/h/"):
			p = "/h.html"
		}
		for _, c := range []string{p, p + ".html", path.Join(p, "index.html")} {
			fp := filepath.Join(root, filepath.FromSlash(c))
			if fi, err := os.Stat(fp); err == nil && !fi.IsDir() {
				serveFile(w, fp, http.StatusOK)
				return
			}
		}
		serveFile(w, filepath.Join(root, "error", "404.html"), http.StatusNotFound)
	})
}

func serveFile(w http.ResponseWriter, fp string, code int) {
	f, err := os.Open(fp)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	ext := filepath.Ext(fp)
	ctype := mime.TypeByExtension(ext)
	switch {
	case ext == ".md":
		ctype = "text/markdown; charset=utf-8"
	case ext == ".txt", ext == ".sh", ext == ".mjs" && strings.Contains(fp, "/tools/"):
		ctype = "text/plain; charset=utf-8"
	case strings.HasSuffix(fp, "api-catalog"):
		ctype = "application/linkset+json"
	case ctype == "":
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = io.Copy(w, f)
}
