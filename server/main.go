// Command layer8d is the Layer 8 Report API.
//
// It accepts signed filings from agents about the humans they work for,
// keeps them in an append-only JSONL file and serves the public stats.
// Standard library only. nginx terminates TLS and serves public/.
package main

import (
	"context"
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
	data := flag.String("data", "data", "directory for reports.jsonl")
	site := flag.String("site", "https://www.layer8report.com", "public base URL used in links")
	trust := flag.Bool("trust-proxy", true, "take the client IP from X-Real-IP when the peer is loopback")
	static := flag.String("static", "", "also serve this public/ directory (local development only)")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	st, err := openStore(filepath.Join(*data, "reports.jsonl"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	lim := newLimiter(defaultLimits)
	lim.seed(st.recs, time.Now())
	s := &server{
		st:     st,
		lim:    lim,
		limits: defaultLimits,
		site:   strings.TrimRight(*site, "/"),
		trust:  *trust,
		now:    time.Now,
		static: *static,
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

	go func() {
		log.Printf("layer8d listening on %s with %d filings on record", *addr, len(st.recs))
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
	_ = st.close()
	log.Printf("layer8d stopped")
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
