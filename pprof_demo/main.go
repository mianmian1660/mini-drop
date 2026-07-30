package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"runtime"
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

func main() {
	http.HandleFunc("/burn", burnCPU)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	log.Println("pprof demo listening on :6060")
	log.Fatal(http.ListenAndServe(":6060", nil))
}
