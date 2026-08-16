package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"sync"
)

func burnCPU(w http.ResponseWriter, r *http.Request) {
	for i := 0; i < 8; i++ {
		go func() {
			x := uint64(1)
			for {
				x = x*1664525 + 1013904223
				runtime.KeepAlive(x)
			}
		}()
	}
	fmt.Fprintln(w, "CPU workers started")
}

// heapAllocations keeps live allocations referenced for heap profiling.
// Calling /alloc triggers a burst of allocations that remain live until
// the server is restarted, so /debug/pprof/heap will report meaningful
// inuse_heap bytes for flamegraph demonstration.
var (
	heapMu         sync.Mutex
	heapAllocations [][]byte
)

func allocHeap(w http.ResponseWriter, r *http.Request) {
	// Allocate ~64MB in 1MB chunks and keep them referenced.
	const chunks = 64
	const chunkSize = 1 << 20 // 1MB
	heapMu.Lock()
	defer heapMu.Unlock()
	for i := 0; i < chunks; i++ {
		buf := make([]byte, chunkSize)
		// Touch all pages so RSS actually grows.
		for j := 0; j < chunkSize; j += 4096 {
			buf[j] = byte(j)
		}
		heapAllocations = append(heapAllocations, buf)
	}
	runtime.GC()
	fmt.Fprintf(w, "allocated %d MB, total live chunks=%d\n", chunks, len(heapAllocations))
}

func main() {
	http.HandleFunc("/burn", burnCPU)
	http.HandleFunc("/alloc", allocHeap)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	log.Println("pprof demo listening on :6060")
	log.Fatal(http.ListenAndServe(":6060", nil))
}
