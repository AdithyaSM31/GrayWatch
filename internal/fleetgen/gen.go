// Package fleetgen renders the Docker Compose file and HAProxy config for a
// fleet of n demo service containers.
package fleetgen

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.tmpl"))

// HostName returns the container name of host i, counting from 1.
func HostName(i int) string {
	return fmt.Sprintf("app-%02d", i)
}

// Render returns the contents of docker-compose.yml and haproxy.cfg.
func Render(n int, healthMode string) (compose []byte, haproxyCfg []byte, err error) {
	if n < 1 {
		return nil, nil, fmt.Errorf("fleet size must be at least 1, got %d", n)
	}
	if healthMode != "shallow" && healthMode != "deep" {
		return nil, nil, fmt.Errorf("health mode must be shallow or deep, got %q", healthMode)
	}

	data := struct {
		Hosts      []string
		HealthMode string
	}{HealthMode: healthMode}
	for i := 1; i <= n; i++ {
		data.Hosts = append(data.Hosts, HostName(i))
	}

	var c, h bytes.Buffer
	if err := templates.ExecuteTemplate(&c, "compose.yml.tmpl", data); err != nil {
		return nil, nil, err
	}
	if err := templates.ExecuteTemplate(&h, "haproxy.cfg.tmpl", data); err != nil {
		return nil, nil, err
	}
	return c.Bytes(), h.Bytes(), nil
}
