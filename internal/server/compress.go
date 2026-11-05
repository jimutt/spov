package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

// compressedReads are the reads that carry a whole conversation, the
// two a long session makes heavy. Nothing else is compressed, because a
// compressed response's size can leak a secret it holds (BREACH). These
// two hold no spov credential, text in them changes only when a run or
// a person writes it, and the browser guard refuses the cross-site
// requests the attack repeats.
//
// @decision(DL-071)
var compressedReads = []string{
	APIPrefix + "sessions/*/messages",
	APIPrefix + "sessions/*/log",
}

// compresses reports whether a request's response is gzipped.
func compresses(r *http.Request) bool {
	if r.Method != http.MethodGet || !acceptsGzip(r.Header.Values("Accept-Encoding")) {
		return false
	}
	for _, pattern := range compressedReads {
		if ok, _ := path.Match(pattern, r.URL.Path); ok {
			return true
		}
	}
	return false
}

// acceptsGzip reads Accept-Encoding for gzip with a weight above zero.
func acceptsGzip(values []string) bool {
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			name, params, _ := strings.Cut(part, ";")
			if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
				continue
			}
			weight, found := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q=")
			if !found {
				return true
			}
			q, err := strconv.ParseFloat(weight, 64)
			return err == nil && q > 0
		}
	}
	return false
}

// gzipWriter compresses a JSON or event-stream response, decided when
// the handler writes its header. Flush flushes the compressor first, so
// each server-sent event reaches the client when it is sent.
//
// @decision(DL-071)
type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

func (w *gzipWriter) WriteHeader(status int) {
	if !w.decided {
		w.decided = true
		h := w.Header()
		kind := h.Get("Content-Type")
		if h.Get("Content-Encoding") == "" && (strings.HasPrefix(kind, "application/json") || strings.HasPrefix(kind, "text/event-stream")) {
			h.Set("Content-Encoding", "gzip")
			h.Add("Vary", "Accept-Encoding")
			h.Del("Content-Length")
			w.gz = gzipWriters.Get().(*gzip.Writer)
			w.gz.Reset(w.ResponseWriter)
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipWriter) Write(b []byte) (int, error) {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	if w.gz == nil {
		return w.ResponseWriter.Write(b)
	}
	return w.gz.Write(b)
}

func (w *gzipWriter) Flush() {
	// Flushing commits the header, so the encoding is decided first.
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *gzipWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// close ends the compressed stream once the handler has returned, and
// returns the compressor to the pool.
func (w *gzipWriter) close() {
	if w.gz != nil {
		_ = w.gz.Close()
		w.gz.Reset(io.Discard)
		gzipWriters.Put(w.gz)
		w.gz = nil
	}
}

// gzipWriters are reused: one costs about a megabyte even at BestSpeed,
// the level chosen because a long log is tens of megabytes of repetitive
// JSON, which it still shrinks severalfold.
var gzipWriters = sync.Pool{New: func() any {
	gz, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return gz
}}

// withCompression gzips the responses compresses picks.
//
// @decision(DL-071)
func withCompression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !compresses(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}
