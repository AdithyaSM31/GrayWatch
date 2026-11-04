// Command dep is the shared HTTP dependency: it sleeps 2 ms and returns 200.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":8081", "listen address")
	flag.Parse()
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(2 * time.Millisecond)
	})))
}
