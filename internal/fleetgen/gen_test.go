package fleetgen

import (
	"bytes"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func render(t *testing.T, n int, mode string) (compose, haproxy string) {
	t.Helper()
	c, h, err := Render(n, mode)
	if err != nil {
		t.Fatalf("Render(%d, %q): %v", n, mode, err)
	}
	return string(c), string(h)
}

func TestHostName(t *testing.T) {
	for i, want := range map[int]string{3: "app-03", 20: "app-20"} {
		if got := HostName(i); got != want {
			t.Errorf("HostName(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestRenderHAProxyServers(t *testing.T) {
	_, haproxy := render(t, 20, "shallow")
	servers := regexp.MustCompile(`(?m)^\s*server app-\d\d app-\d\d:8080 check$`).FindAllString(haproxy, -1)
	if len(servers) != 20 {
		t.Fatalf("got %d server lines, want 20:\n%s", len(servers), haproxy)
	}
}

func TestRenderHAProxyHealthAndTimeouts(t *testing.T) {
	_, haproxy := render(t, 20, "shallow")
	for _, want := range []string{
		"backend be_app",
		"balance roundrobin",
		"option httpchk GET /health",
		"inter 2s fall 3 rise 2",
		"timeout connect 1s",
		"timeout server 2s",
		`log-format "gw %s %Tr %ST"`,
		"log host.docker.internal:5140 local0",
		"stats socket ipv4@0.0.0.0:9999 level admin",
	} {
		if !strings.Contains(haproxy, want) {
			t.Errorf("haproxy.cfg is missing %q", want)
		}
	}
}

func TestRenderComposeHealthMode(t *testing.T) {
	compose, _ := render(t, 20, "deep")
	if got := strings.Count(compose, "HEALTH_MODE: deep"); got != 20 {
		t.Fatalf("HEALTH_MODE: deep appears %d times, want 20", got)
	}
}

type composeFile struct {
	Services map[string]struct {
		Image         string            `yaml:"image"`
		Command       string            `yaml:"command"`
		ContainerName string            `yaml:"container_name"`
		Hostname      string            `yaml:"hostname"`
		Labels        map[string]string `yaml:"labels"`
		CPUs          float64           `yaml:"cpus"`
		MemLimit      string            `yaml:"mem_limit"`
		Volumes       []string          `yaml:"volumes"`
		Environment   map[string]string `yaml:"environment"`
		Ports         []string          `yaml:"ports"`
		ExtraHosts    []string          `yaml:"extra_hosts"`
		DependsOn     []string          `yaml:"depends_on"`
	} `yaml:"services"`
}

func parseCompose(t *testing.T, n int) composeFile {
	t.Helper()
	compose, _ := render(t, n, "shallow")
	var f composeFile
	if err := yaml.Unmarshal([]byte(compose), &f); err != nil {
		t.Fatalf("compose is not valid YAML: %v\n%s", err, compose)
	}
	return f
}

func TestRenderComposeAppServices(t *testing.T) {
	f := parseCompose(t, 20)
	if len(f.Services) != 23 {
		t.Fatalf("got %d services, want 23 (20 apps, postgres, dep, haproxy)", len(f.Services))
	}
	app, ok := f.Services["app-07"]
	if !ok {
		t.Fatal("no app-07 service")
	}
	if app.Image != "graywatch/app" || app.ContainerName != "app-07" || app.Hostname != "app-07" {
		t.Errorf("image/container_name/hostname = %q/%q/%q", app.Image, app.ContainerName, app.Hostname)
	}
	if app.Labels["graywatch.role"] != "app" {
		t.Errorf("labels = %v, want graywatch.role=app", app.Labels)
	}
	if app.CPUs != 0.5 || app.MemLimit != "512m" {
		t.Errorf("cpus/mem_limit = %v/%q, want 0.5/512m", app.CPUs, app.MemLimit)
	}
	if !slices.Contains(app.Volumes, "./data/app-07:/data") {
		t.Errorf("volumes = %v, want ./data/app-07:/data", app.Volumes)
	}
	if app.Environment["HEALTH_MODE"] != "shallow" || app.Environment["DB_URL"] == "" {
		t.Errorf("environment = %v", app.Environment)
	}
}

// The admin port carries the fault hooks and must never leave the Docker
// network; only HAProxy publishes anything.
func TestRenderComposePublishesOnlyHAProxy(t *testing.T) {
	f := parseCompose(t, 20)
	for name, svc := range f.Services {
		if name != "haproxy" && len(svc.Ports) != 0 {
			t.Errorf("%s publishes ports %v", name, svc.Ports)
		}
	}
	ports := f.Services["haproxy"].Ports
	if !slices.Contains(ports, "8000:8000") || !slices.Contains(ports, "127.0.0.1:9999:9999") {
		t.Errorf("haproxy ports = %v", ports)
	}
	if !slices.Contains(f.Services["haproxy"].ExtraHosts, "host.docker.internal:host-gateway") {
		t.Errorf("haproxy extra_hosts = %v", f.Services["haproxy"].ExtraHosts)
	}
}

// HAProxy resolves server names once at startup, so every app must exist first.
func TestRenderComposeHAProxyStartsAfterApps(t *testing.T) {
	f := parseCompose(t, 20)
	for i := 1; i <= 20; i++ {
		if !slices.Contains(f.Services["haproxy"].DependsOn, HostName(i)) {
			t.Errorf("haproxy does not depend on %s", HostName(i))
		}
	}
}

// 20 hosts with a pool of 10 can open 200 connections; the default limit is 100.
func TestRenderComposePostgresAllowsWholeFleet(t *testing.T) {
	f := parseCompose(t, 20)
	pg := f.Services["postgres"]
	if pg.Image != "postgres:16" || pg.Environment["POSTGRES_PASSWORD"] != "gw" {
		t.Errorf("postgres image/env = %q/%v", pg.Image, pg.Environment)
	}
	if !strings.Contains(pg.Command, "max_connections=300") {
		t.Errorf("postgres command = %q, want max_connections=300", pg.Command)
	}
}

// The dep-slow fault looks this container up by the name "dep".
func TestRenderComposeDepContainerName(t *testing.T) {
	if got := parseCompose(t, 20).Services["dep"].ContainerName; got != "dep" {
		t.Errorf("dep container_name = %q, want dep", got)
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	if _, _, err := Render(0, "shallow"); err == nil {
		t.Error("Render(0, shallow) succeeded, want error")
	}
	if _, _, err := Render(20, "medium"); err == nil {
		t.Error("Render(20, medium) succeeded, want error")
	}
}

func TestRenderHAProxyGolden(t *testing.T) {
	_, haproxy, err := Render(3, "shallow")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/haproxy.golden.cfg")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(haproxy, want) {
		t.Fatalf("haproxy.cfg differs from golden file\ngot:\n%s\nwant:\n%s", haproxy, want)
	}
}
