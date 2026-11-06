// Command fleetgen writes docker-compose.yml and haproxy.cfg for the fleet.
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/AdithyaSM31/graywatch/internal/fleetgen"
)

func main() {
	n := flag.Int("n", 20, "number of app containers")
	health := flag.String("health", "shallow", "health check mode: shallow or deep")
	out := flag.String("out", "deploy", "output directory")
	flag.Parse()

	compose, haproxyCfg, err := fleetgen.Render(*n, *health)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*out, "docker-compose.yml"), compose, 0o644); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*out, "haproxy.cfg"), haproxyCfg, 0o644); err != nil {
		log.Fatal(err)
	}
}
