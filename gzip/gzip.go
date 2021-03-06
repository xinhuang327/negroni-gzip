// Package gzip implements a gzip compression handler middleware for Negroni.
package gzip

import (
	"io/ioutil"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"

	// "github.com/klauspost/compress/gzip"
	"compress/gzip"

	"github.com/urfave/negroni"
)

// These compression constants are copied from the compress/gzip package.
const (
	encodingGzip = "gzip"

	headerAcceptEncoding  = "Accept-Encoding"
	headerContentEncoding = "Content-Encoding"
	headerContentLength   = "Content-Length"
	headerContentType     = "Content-Type"
	headerVary            = "Vary"
	headerSecWebSocketKey = "Sec-WebSocket-Key"

	BestCompression    = gzip.BestCompression
	BestSpeed          = gzip.BestSpeed
	DefaultCompression = gzip.DefaultCompression
	NoCompression      = gzip.NoCompression
)

// gzipResponseWriter is the ResponseWriter that negroni.ResponseWriter is
// wrapped in.
// Should add a bytes.Buffer for get out the cache bytes.
type gzipResponseWriter struct {
	w *gzip.Writer
	negroni.ResponseWriter
	wroteHeader bool
}

// Check whether underlying response is already pre-encoded and disable
// gzipWriter before the body gets written, otherwise encoding headers
func (grw *gzipResponseWriter) WriteHeader(code int) {
	headers := grw.ResponseWriter.Header()
	if headers.Get(headerContentEncoding) == "" {
		headers.Set(headerContentEncoding, encodingGzip)
		headers.Add(headerVary, headerAcceptEncoding)
	} else {
		grw.w.Reset(ioutil.Discard)
		grw.w = nil
	}

	// Avoid sending Content-Length header before compression. The length would
	// be invalid, and some browsers like Safari will report
	// "The network connection was lost." errors
	grw.Header().Del(headerContentLength)

	grw.ResponseWriter.WriteHeader(code)
	grw.wroteHeader = true
}

// Write writes bytes to the gzip.Writer. It will also set the Content-Type
// header using the net/http library content type detection if the Content-Type
// header was not set yet.
func (grw *gzipResponseWriter) Write(b []byte) (int, error) {
	if !grw.wroteHeader {
		grw.WriteHeader(http.StatusOK)
	}
	if grw.w == nil {
		return grw.ResponseWriter.Write(b)
	}
	if len(grw.Header().Get(headerContentType)) == 0 {
		grw.Header().Set(headerContentType, http.DetectContentType(b))
	}
	return grw.w.Write(b) // .w is the Gzip writer wrapping .ResponseWriter
}

type gzipResponseWriterCloseNotifier struct {
	*gzipResponseWriter
}

func (rw *gzipResponseWriterCloseNotifier) CloseNotify() <-chan bool {
	return rw.ResponseWriter.(http.CloseNotifier).CloseNotify()
}

func newGzipResponseWriter(rw negroni.ResponseWriter, w *gzip.Writer) negroni.ResponseWriter {
	wr := &gzipResponseWriter{w: w, ResponseWriter: rw}

	if _, ok := rw.(http.CloseNotifier); ok {
		return &gzipResponseWriterCloseNotifier{gzipResponseWriter: wr}
	}

	return wr
}

// handler struct contains the ServeHTTP method
type handler struct {
	pool            sync.Pool
	skipExtensions  []string
	cacheExtensions []string
}

// Gzip returns a handler which will handle the Gzip compression in ServeHTTP.
// Valid values for level are identical to those in the compress/gzip package.
func Gzip(level int) *handler {
	return GzipWithOptions(level, nil, nil)
}

func GzipWithOptions(level int, skipExtensions []string, cacheExtensions []string) *handler {
	h := &handler{}
	h.pool.New = func() interface{} {
		gz, err := gzip.NewWriterLevel(ioutil.Discard, level)
		if err != nil {
			panic(err)
		}
		return gz
	}
	// We can skip gzip for files like "png", "jpg", "woff2", "mp3", "mp4", "mov"
	h.skipExtensions = skipExtensions
	// We can cache gzip result for static files like "js", "css"
	h.cacheExtensions = cacheExtensions
	return h
}

// ServeHTTP wraps the http.ResponseWriter with a gzip.Writer.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {

	skipForExtension := false
	urlExt := path.Ext(r.URL.Path)

	if len(h.skipExtensions) > 0 {
		for _, ext := range h.skipExtensions {
			if urlExt == "."+ext {
				skipForExtension = true
				break
			}
		}
	}

	useCache := false
	var cacheKey string

	if len(h.cacheExtensions) > 0 {
		for _, ext := range h.cacheExtensions {
			if urlExt == "."+ext {
				useCache = true
				cacheKey = filepath.Base(r.URL.Path)
				break
			}
		}
	}

	// Skip compression if the client doesn't accept gzip encoding.
	if !strings.Contains(r.Header.Get(headerAcceptEncoding), encodingGzip) || skipForExtension {
		next(w, r)
		return
	}

	// Skip compression if client attempt WebSocket connection
	if len(r.Header.Get(headerSecWebSocketKey)) > 0 {
		next(w, r)
		return
	}

	// Retrieve gzip writer from the pool. Reset it to use the ResponseWriter.
	// This allows us to re-use an already allocated buffer rather than
	// allocating a new buffer for every request.
	// We defer g.pool.Put here so that the gz writer is returned to the
	// pool if any thing after here fails for some reason (functions in
	// next could potentially panic, etc)
	gz := h.pool.Get().(*gzip.Writer)
	defer h.pool.Put(gz)
	gz.Reset(w)

	// Wrap the original http.ResponseWriter with negroni.ResponseWriter
	// and create the gzipResponseWriter.
	nrw := negroni.NewResponseWriter(w)
	grw := newGzipResponseWriter(nrw, gz)

	if useCache {
		nrw.Header().Set("X-GzipCacheKey", cacheKey)
	}

	// Call the next handler supplying the gzipResponseWriter instead of
	// the original.
	next(grw, r)

	gz.Close()
}
