package htmw

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	htMetrics "github.com/slok/go-http-metrics/metrics/prometheus"
	"github.com/slok/go-http-metrics/middleware"
)

func HandleHTTPMetrics(reg *prometheus.Registry) func(http.Handler) http.Handler {
	metricsMw := middleware.New(
		middleware.Config{
			Recorder: htMetrics.NewRecorder(htMetrics.Config{Registry: reg}),
		})

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wi := &responseWriterInterceptor{
				statusCode:     http.StatusOK,
				ResponseWriter: w,
			}
			reporter := &muxReporter{
				w: wi,
				r: r,
			}

			metricsMw.Measure("", reporter, func() {
				h.ServeHTTP(wi, r)
			})
		})
	}
}

type muxReporter struct {
	w *responseWriterInterceptor
	r *http.Request
}

func (m *muxReporter) Method() string { return m.r.Method }

func (m *muxReporter) Context() context.Context { return m.r.Context() }

func (m *muxReporter) URLPath() string {
	path, err := mux.CurrentRoute(m.r).GetPathTemplate()
	if err != nil {
		return m.r.URL.Path
	}
	return path
}

func (m *muxReporter) StatusCode() int { return m.w.statusCode }

func (m *muxReporter) BytesWritten() int64 { return int64(m.w.bytesWritten) }

// responseWriterInterceptor is a simple wrapper to intercept set data on a
// ResponseWriter.
type responseWriterInterceptor struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
}

func (w *responseWriterInterceptor) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *responseWriterInterceptor) Write(p []byte) (int, error) {
	w.bytesWritten += len(p)
	return w.ResponseWriter.Write(p)
}

func (w *responseWriterInterceptor) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("type assertion failed http.ResponseWriter not a http.Hijacker")
	}
	return h.Hijack()
}

func (w *responseWriterInterceptor) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	f.Flush()
}
