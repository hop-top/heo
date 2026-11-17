//go:build heoscale

package composition

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"

	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
)

// Environment the scale harness build reads. Release builds carry none
// of it.
const (
	// ScaleGmailEnv is the Gmail API root the google.mail adapter uses.
	ScaleGmailEnv = "HEO_SCALE_GMAIL_ENDPOINT"
	// ScaleGraphEnv is the Graph root the microsoft.mail adapter uses.
	ScaleGraphEnv = "HEO_SCALE_GRAPH_ENDPOINT"
	// ScaleDebugEnv is the address the process serves /stats and pprof
	// on.
	ScaleDebugEnv = "HEO_SCALE_DEBUG_ADDR"
)

func googleOptions() google.Options { return google.Options{Endpoint: os.Getenv(ScaleGmailEnv)} }

func microsoftOptions() microsoft.Options {
	return microsoft.Options{Endpoint: os.Getenv(ScaleGraphEnv)}
}

func init() {
	addr := os.Getenv(ScaleDebugEnv)
	if addr == "" {
		return
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		_ = json.NewEncoder(w).Encode(map[string]uint64{
			"goroutines": uint64(runtime.NumGoroutine()),
			"heap_alloc": m.HeapAlloc, "heap_inuse": m.HeapInuse, "heap_sys": m.HeapSys,
			"sys": m.Sys, "total_alloc": m.TotalAlloc, "num_gc": uint64(m.NumGC),
		})
	})
	go func() { _ = http.Serve(l, mux) }() //nolint:gosec // loopback debug listener of the scale build
}
