// Command loadgen drives open-loop load and prints latency and error rate.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/loadgen"
)

func main() {
	url := flag.String("url", "http://localhost:8000/work", "target URL")
	rps := flag.Int("rps", 600, "requests per second")
	duration := flag.Duration("duration", 60*time.Second, "how long to send requests")
	timeout := flag.Duration("timeout", 2*time.Second, "per-request timeout")
	out := flag.String("out", "", "write every result to this CSV file")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()

	results := make(chan loadgen.Result, 1024)
	errc := make(chan error, 1)
	go func() {
		errc <- loadgen.Run(ctx, loadgen.Config{URL: *url, RPS: *rps, Timeout: *timeout}, results)
	}()
	var rs []loadgen.Result
	for r := range results {
		rs = append(rs, r)
	}
	if err := <-errc; err != nil {
		log.Fatal(err)
	}

	s := loadgen.Summarize(rs)
	fmt.Printf("requests=%d p50=%v p99=%v error_rate=%.4f%%\n", s.N, s.P50, s.P99, s.ErrRate*100)

	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		if err := loadgen.WriteCSV(f, rs); err != nil {
			log.Fatal(err)
		}
	}
}
