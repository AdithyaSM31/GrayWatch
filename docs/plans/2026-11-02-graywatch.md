# Graywatch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Find containers that pass their HTTP health check but are actually degraded ("gray failures"), take them out of the load balancer without ever pulling too much of the fleet, and prove it with a fault-injection benchmark.

**Architecture:** 20 app containers sit behind HAProxy on one Linux VM. A root daemon, `graywatchd`, runs on the host. Every second it reads two kinds of data: kernel signals for each container (cgroup v2 PSI pressure, CPU throttling, TCP retransmits per network namespace) and client-side signals (HAProxy request logs). It compares each container against the rest of the fleet using a robust z-score, debounces the result, and drains outliers through the HAProxy runtime API, with blast-radius guards. A chaos CLI injects 8 real fault types plus one fleet-wide control. A bench harness scores Graywatch against shallow and deep HTTP health checks.

**Tech Stack:** Go (single module), Docker Engine + Compose, HAProxy 2.8, PostgreSQL 16, Linux cgroup v2 / PSI / iproute2 `tc netem` / `nsenter`, Prometheus + Grafana (polish only), optional eBPF via `cilium/ebpf`.

**Spec:** See the Design section below. This is the design agreed in chat on 2026-11-02; there is no separate spec file.

**Schedule (3 weeks):** Week 1 covers Tasks 1–5 (fleet, load, host introspection, chaos). Week 2 covers Tasks 6–9 (HAProxy I/O, collector, detector, remediation daemon). Week 3 covers Tasks 10–11 (benchmark, results, dashboard, README). Task 12 (eBPF) is a stretch goal and the project is complete without it.

---

## Design

```
            loadgen (600 rps, open-loop)
                     │
                     ▼
               ┌──────────┐  runtime API 127.0.0.1:9999 (show stat / set server state)
               │ HAProxy  │◄──────────────────────────────────────────────┐
               │  be_app  │── UDP request log "gw %s %Tr %ST" → :5140 ──┐  │
               └────┬─────┘                                             │  │
     health check   │  round robin                                      ▼  │
   GET /health 2s   ▼                                             ┌───────────┐
   ┌────────┬────────┬─────── … ───────┐                          │graywatchd │ (root, on host)
   │ app-01 │ app-02 │       app-20    │── cgroup v2 files ──────►│ collect   │
   └───┬────┴───┬────┴─────── … ───────┘── /proc/<pid>/net/snmp ►│ detect    │── events.jsonl
       │        │                                                 │ remediate │── :9100 /metrics
       ▼        ▼                                                 └───────────┘
   postgres (shared pool)   dep (HTTP, resolved per request)
```

- **App `/work` request:** fsync a 4 KB audit record to `/data/audit.log`, run one Postgres query from a pool of 10 with a 1 s acquire timeout, make one HTTP GET to `dep` (no keep-alive, so DNS is resolved every time), and burn about 3 ms of CPU. Any failure returns 500.
- **App `/health`:** `shallow` mode always returns 200, like most real services do. `deep` mode checks the DB and dep and returns 503 if either fails.
- **Signals, all computed every 1 s tick:** `lat_p90_ms`, `err_rate` (client-side, 5 s sliding window), `cpu_psi`, `io_psi`, `mem_psi`, `cpu_throttle`, `tcp_retrans` (kernel-side, per-tick deltas).
- **Detection:** compare each host to its peers, not to fixed thresholds. A host is Suspect when it is flagged in ≥3 of the last 5 ticks. A fleet-wide SLO breach raises a FleetAlert: it means a shared dependency is broken, so no host gets drained.
- **Remediation:** drain via HAProxy, with these limits: never drain more than 10% of the fleet, never drain while more than 30% of hosts look sick or a FleetAlert is active, at most one action per 10 s, cooldown before restoring, and flap protection.

## Global Constraints

- Go ≥ 1.22, module `github.com/AdithyaSM31/graywatch`. The only third-party deps allowed are `github.com/jackc/pgx/v5`, `github.com/prometheus/client_golang`, `gopkg.in/yaml.v3` and `golang.org/x/sys`.
- Host: a real Linux VM (Ubuntu 24.04, EC2 `c6i.xlarge` or a local VM). Docker Desktop and WSL won't work. Requirements: `stat -fc %T /sys/fs/cgroup` prints `cgroup2fs`, `cat /proc/pressure/cpu` succeeds, and `tc`, `nsenter` and `socat` are installed. App data dirs must sit on a block device, not tmpfs.
- Fleet: `N = 20` containers named `app-01` … `app-20`, label `graywatch.role=app`, `cpus: 0.5`, `mem_limit: 512m`. App ports: 8080 (traffic) and 9090 (admin/fault hooks, never published).
- HAProxy: backend `be_app`, `balance roundrobin`, `option httpchk GET /health`, `default-server inter 2s fall 3 rise 2`, `timeout connect 1s`, `timeout server 2s`. Runtime API: `stats socket ipv4@0.0.0.0:9999 level admin`, published only to `127.0.0.1:9999`. Log: `log host.docker.internal:5140 local0` with `log-format "gw %s %Tr %ST"`.
- `graywatchd` runs as root on the host with a tick of `1s`.
- Detector defaults: `Z=3.5`, `Window=5`, `Need=3`, `MinHosts=5`, `FleetLatencyMs=300`, `FleetErrRate=0.05`.

  | Signal | MinMAD | MinDelta | Category |
  |---|---|---|---|
  | `lat_p90_ms` | 5 | 50 | app |
  | `err_rate` | 0.005 | 0.02 | app |
  | `cpu_psi` | 0.01 | 0.10 | cpu |
  | `cpu_throttle` | 0.01 | 0.10 | cpu |
  | `io_psi` | 0.01 | 0.10 | disk |
  | `mem_psi` | 0.01 | 0.05 | memory |
  | `tcp_retrans` | 0.002 | 0.01 | network |

- Remediation defaults: `MaxDrainFrac=0.10` (cap = `max(1, floor(0.10·N))`), `FleetWideFrac=0.30`, `Cooldown=60s`, `MinActionGap=10s`, `MaxDrainsPerWindow=3` per host per `10m`.
- Load: open-loop at `600 rps` with a `2s` client timeout. Latency is measured from the *scheduled* send time, so coordinated omission doesn't hide slow requests.
- `go test ./...` must pass on any dev machine. Tests that need root, Docker or a real kernel use the build tag `integration` and run only on the VM.

## Review Focus

1. **A container restarts or a counter goes backwards:** the PID changes and the cgroup/snmp counters reset. The collector must drop that tick's delta (never emit a negative or huge rate) and rediscover the host. → Task 7 `TestCounterResetSkipsSample`.
2. **A very uniform fleet (MAD = 0):** for example `err_rate` is exactly 0 on 19 hosts. The z-score must not become Inf or NaN, and one host at 0.03 must still be flagged. → Task 8 `TestRobustZ_MADZeroUsesFloor`.
3. **A drained host has no traffic:** its client-side signals disappear. That is neither healthy evidence nor an outlier, so judge it only on kernel signals. → Task 8 `TestMissingClientSignalsNotEvidence`.
4. **The HAProxy runtime API fails during a drain:** the host must not be recorded as drained, the cap must still hold, and the drain is retried on the next tick. → Task 9 `TestActuatorFailureNotCountedAsDrained`.
5. **Chaos is left applied after a crash or Ctrl-C:** a throttled container must not survive into the next experiment. → Task 5 `TestRevertAllUsesStateFiles`, Task 10 SIGINT step.

---

## File Structure

```
cmd/{svc,dep,fleetgen,loadgen,chaos,graywatchd,bench}/main.go   thin flag parsing + wiring only
internal/svc/         demo service: handlers, pool wrapper, admin fault hooks
internal/fleetgen/    renders docker-compose.yml + haproxy.cfg from templates
internal/loadgen/     open-loop load generator + percentile helper
internal/cgroup/      PSI, cpu.stat, uint files, /proc/<pid>/cgroup → cgroup dir, block device lookup
internal/netstat/     /proc/<pid>/net/snmp TCP counters
internal/fleet/       Docker discovery (via docker CLI) → []Host
internal/chaos/       Fault interface, 9 faults, state files, command runner
internal/haproxy/     runtime API client (show stat, set server state)
internal/lblog/       UDP syslog listener + per-server sliding window
internal/collect/     turns snapshots into per-tick Samples
internal/detect/      robust z, debounce, categories, fleet alert
internal/remediate/   pure planner + HAProxy actuator
internal/events/      Event type + JSONL writer/reader (shared by daemon and bench)
internal/metrics/     Prometheus gauges/counters
internal/bench/       scenarios, scoring, orchestration, report
deploy/               generated compose/haproxy + prometheus.yml + grafana/
bench/scenarios/      one YAML per fault
scripts/vm-setup.sh   preflight checks
docs/RESULTS.md       generated benchmark results
```

---

### Task 1: Demo service (`svc`) and dependency (`dep`)

**Files:**
- Create: `go.mod`, `cmd/svc/main.go`, `cmd/dep/main.go`, `internal/svc/server.go`, `internal/svc/pool.go`, `internal/svc/admin.go`, `Dockerfile`
- Test: `internal/svc/server_test.go`, `internal/svc/admin_test.go`

**Interfaces:**
- Produces:
  - `type Pool interface { Query(ctx context.Context) error; Ping(ctx context.Context) error; Hold(n int) error; Held() int }`, with the real implementation `NewPGPool(ctx, url string, size int32) (Pool, error)` built on `pgxpool` (`MaxConns=size`, acquire timeout 1 s inside `Query`).
  - `type Config struct { Hostname, DepURL, HealthMode, AuditPath string; WorkCPU time.Duration }`
  - `func NewServer(cfg Config, pool Pool, dep *http.Client) http.Handler` with routes `GET /work`, `GET /health`. Every response sets the header `X-Served-By: <Hostname>`.
  - `func NewAdmin(pool Pool, burner *Burner, hog *MemHog) http.Handler` with routes `POST /admin/leak?n=`, `POST /admin/burn?threads=`, `POST /admin/memhog?mb=`. Each returns 200 with a JSON body of its current state.
  - `type Burner struct{…}; func (b *Burner) Set(threads int); func (b *Burner) Active() int`
  - `type MemHog struct{…}; func (m *MemHog) Set(mb int); func (m *MemHog) Held() int`. Allocate `[]byte` chunks and write one byte per 4 KiB page so the memory is really resident.
  - Env vars for `cmd/svc`: `PORT=8080 ADMIN_PORT=9090 DB_URL DEP_URL=http://dep:8081/ HEALTH_MODE=shallow|deep AUDIT_PATH=/data/audit.log POOL_SIZE=10 WORK_CPU_MS=3`.
  - `cmd/dep`: listens on `:8081`, sleeps 2 ms, returns 200.
  - `Dockerfile`: multi-stage, `CGO_ENABLED=0` (this forces Go's pure resolver, which re-reads `resolv.conf` and is what the dns-break fault relies on), final image `alpine:3.20`, one image containing both binaries.

- [ ] **Step 1: Write the failing tests** with a `fakePool` (a semaphore of size 10) and an `httptest` dep server.

```go
func TestWorkSetsServedBy(t *testing.T)            // header X-Served-By == "app-07", status 200
func TestWorkReturns500WhenPoolExhausted(t *testing.T) {
    // pool.Hold(10); GET /work → 500; elapsed < 1500*time.Millisecond
}
func TestWorkReturns500WhenDepDown(t *testing.T)   // dep server closed → 500
func TestHealthShallowIgnoresDeps(t *testing.T)    // pool.Hold(10), dep closed, HealthMode "shallow" → 200
func TestHealthDeepFailsWhenPoolExhausted(t *testing.T) // HealthMode "deep", pool.Hold(10) → 503
func TestLeakHoldsAndReleases(t *testing.T)        // POST /admin/leak?n=9 → pool.Held()==9; n=0 → 0
func TestBurnerSetAndStop(t *testing.T)            // Set(2) → Active()==2; Set(0) → Active()==0 within 1s
func TestMemHogHoldsAndReleases(t *testing.T)      // Set(8) → Held()==8; Set(0) → Held()==0
```

- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/svc/...`. Expected: FAIL (undefined: NewServer…).
- [ ] **Step 3: Implement `pool.go`, `server.go`, `admin.go` and both `main.go` files** to the interfaces above. The audit write is `f.Write(4096 bytes); f.Sync()` on a file opened once with `O_APPEND`. The dep client is `&http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}`.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `go test ./internal/svc/...`. Expected: PASS.
- [ ] **Step 5: Check the image builds.** Run `docker build -t graywatch/app .`. Expected: success, and `docker run --rm graywatch/app /svc -h` prints usage.
- [ ] **Step 6: Commit.** `git commit -m "feat(svc): demo service with shallow/deep health and fault hooks"`

---

### Task 2: Fleet generator, HAProxy config and VM preflight

**Files:**
- Create: `internal/fleetgen/gen.go`, `internal/fleetgen/templates/compose.yml.tmpl`, `internal/fleetgen/templates/haproxy.cfg.tmpl`, `cmd/fleetgen/main.go`, `scripts/vm-setup.sh`
- Test: `internal/fleetgen/gen_test.go`, `internal/fleetgen/testdata/haproxy.golden.cfg`

**Interfaces:**
- Consumes: the image `graywatch/app` from Task 1.
- Produces:
  - `func Render(n int, healthMode string) (compose []byte, haproxyCfg []byte, err error)`. Templates are embedded with `//go:embed`.
  - `func HostName(i int) string`, which returns `fmt.Sprintf("app-%02d", i)` for i = 1..n.
  - CLI: `fleetgen -n 20 -health shallow -out deploy/` writes `deploy/docker-compose.yml` and `deploy/haproxy.cfg`.
  - Compose services: `postgres:16` (`POSTGRES_PASSWORD=gw`), `dep`, `app-01..app-NN` (`container_name`=`hostname`=name, label `graywatch.role=app`, `cpus: 0.5`, `mem_limit: 512m`, volume `./data/<name>:/data`, env `HEALTH_MODE`), and `haproxy:2.8` (ports `8000:8000`, `127.0.0.1:9999:9999`; `extra_hosts: ["host.docker.internal:host-gateway"]`). All config values come from Global Constraints.

- [ ] **Step 1: Write the failing tests.**

```go
func TestHostName(t *testing.T)                    // HostName(3)=="app-03", HostName(20)=="app-20"
func TestRenderHAProxyServers(t *testing.T)        // 20 lines matching `^\s*server app-\d\d app-\d\d:8080 check$`
func TestRenderHAProxyHealthAndTimeouts(t *testing.T) // contains "option httpchk GET /health", "inter 2s fall 3 rise 2", "timeout server 2s", `log-format "gw %s %Tr %ST"`
func TestRenderComposeHealthMode(t *testing.T)     // Render(20,"deep") compose contains HEALTH_MODE: deep 20 times
func TestRenderRejectsBadInput(t *testing.T)       // Render(0,"shallow") and Render(20,"medium") → error
func TestRenderHAProxyGolden(t *testing.T)         // Render(3,"shallow") == testdata/haproxy.golden.cfg
```

- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/fleetgen/...`. Expected: FAIL.
- [ ] **Step 3: Implement `Render`, `HostName` and the CLI**, then write the golden file from a reviewed `Render(3,"shallow")` output.
- [ ] **Step 4: Run the tests and confirm they pass.** Expected: PASS.
- [ ] **Step 5: Write `scripts/vm-setup.sh`.** It exits non-zero with a clear message if any of these fail: `stat -fc %T /sys/fs/cgroup` = `cgroup2fs`, `cat /proc/pressure/cpu`, `command -v tc nsenter socat docker`, `id -u` = 0.
- [ ] **Step 6: Smoke test on the VM.**
  - `sudo scripts/vm-setup.sh && go run ./cmd/fleetgen -n 20 -out deploy && docker compose -f deploy/docker-compose.yml up -d --build`
  - `for i in $(seq 60); do curl -s -D - -o /dev/null localhost:8000/work | grep -i x-served-by; done | sort -u | wc -l` should print `20`.
  - `echo "show stat" | socat stdio tcp:127.0.0.1:9999 | grep -c '^be_app,app-'` should print `20`.
- [ ] **Step 7: Commit.** `git commit -m "feat(fleetgen): compose + haproxy generation, VM preflight"`

---

### Task 3: Open-loop load generator

**Files:**
- Create: `internal/loadgen/loadgen.go`, `internal/loadgen/stats.go`, `cmd/loadgen/main.go`
- Test: `internal/loadgen/loadgen_test.go`, `internal/loadgen/stats_test.go`

**Interfaces:**
- Produces:
  - `type Result struct { Scheduled time.Time; Latency time.Duration; Status int; ServedBy string; Err string }`. `Status` is 0 on a transport error or timeout.
  - `type Config struct { URL string; RPS int; Timeout time.Duration }`
  - `func Run(ctx context.Context, cfg Config, out chan<- Result) error`. It fires request *k* at `start + k·(1s/RPS)` in its own goroutine whether or not earlier requests have finished. `Latency = completion − Scheduled`. It closes `out` when `ctx` ends and all in-flight requests have finished.
  - `func Percentile(ds []time.Duration, p float64) time.Duration` uses nearest rank: sort, then index `ceil(p·n)−1`. It returns 0 for empty input.
  - `func WriteCSV(w io.Writer, rs []Result) error` with header `scheduled_unix_ns,latency_us,status,served_by,err`.
  - CLI: `loadgen -url http://localhost:8000/work -rps 600 -duration 60s -out run.csv`, which prints p50/p99/error rate at the end.

- [ ] **Step 1: Write the failing tests.**

```go
func TestPercentileNearestRank(t *testing.T) // 1..100ms → p0.99 == 99ms, p0.5 == 50ms; empty → 0
func TestRunHitsTargetRate(t *testing.T)     // httptest 200 OK; RPS 200 for 2s → 380 ≤ len(results) ≤ 420
func TestTimeoutRecordedAsError(t *testing.T)// server sleeps 300ms, Timeout 100ms → Status==0, Err!="", Latency ≥ 100ms
func TestLatencyFromScheduledTime(t *testing.T) // server holds a mutex for 50ms per request (serialized), RPS 100 for 1s
                                                // → Percentile(lat, 0.99) > 500ms (proves queueing is counted)
```

- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/loadgen/...`. Expected: FAIL.
- [ ] **Step 3: Implement `Run`, `Percentile`, `WriteCSV` and the CLI.**
- [ ] **Step 4: Run the tests and confirm they pass.** Expected: PASS.
- [ ] **Step 5: Check against the fleet on the VM.** Run `go run ./cmd/loadgen -rps 600 -duration 60s`. Expected: error rate < 0.1% and p99 < 50 ms. Record the baseline numbers in your notes; they become the "normal" reference.
- [ ] **Step 6: Commit.** `git commit -m "feat(loadgen): open-loop generator with coordinated-omission-safe latency"`

---

### Task 4: Host introspection (cgroup, netstat, discovery)

**Files:**
- Create: `internal/cgroup/psi.go`, `internal/cgroup/cpustat.go`, `internal/cgroup/path.go`, `internal/cgroup/blockdev.go`, `internal/netstat/snmp.go`, `internal/fleet/discover.go`
- Test: `internal/cgroup/*_test.go` with `testdata/`, `internal/netstat/snmp_test.go` with `testdata/snmp`, `internal/fleet/discover_test.go`

**Interfaces:**
- Produces:
  - `type PSI struct { SomeTotal, FullTotal time.Duration }` and `func ReadPSI(path string) (PSI, error)`. `total=` is in microseconds. A missing `full` line is fine and gives `FullTotal=0`.
  - `type CPUStat struct { UsageUsec, NrPeriods, NrThrottled, ThrottledUsec uint64 }` and `func ReadCPUStat(path string) (CPUStat, error)`
  - `func ReadUint(path string) (uint64, error)` (for `memory.current`)
  - `func DirForPID(procRoot string, pid int) (string, error)`. It reads `<procRoot>/<pid>/cgroup`, finds the `0::` line and returns `/sys/fs/cgroup` + path. If there is no `0::` line it returns the error `"cgroup v1 not supported"`.
  - `func WholeDiskDevice(sysRoot, path string) (string, error)`. It returns `"MAJ:MIN"` of the *whole disk* holding `path`. Stat the path, take major/minor via `golang.org/x/sys/unix`, and if `<sysRoot>/dev/block/MAJ:MIN/partition` exists, read `../dev`, because `io.max` rejects partitions.
  - `type TCPCounters struct { OutSegs, RetransSegs uint64 }` and `func ReadTCPCounters(snmpPath string) (TCPCounters, error)`. It pairs the two `Tcp:` lines by header name rather than column position.
  - `type Host struct { Name string; PID int; CgroupDir, ResolvConf, IP string }`
  - `type Docker interface { ListByLabel(ctx context.Context, label string) ([]string, error); Inspect(ctx context.Context, name string) (pid int, resolvConf, ip string, err error) }`. The real `CLIDocker` shells out to `docker ps --filter label=… --format '{{.Names}}'` and `docker inspect -f '{{.State.Pid}} {{.ResolvConfPath}} {{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'`.
  - `func Discover(ctx context.Context, d Docker, procRoot string) ([]Host, error)` returns hosts sorted by name. `func Lookup(ctx context.Context, d Docker, procRoot, name string) (Host, error)` resolves any container, including `dep`.

- [ ] **Step 1: Write the failing tests** against fixtures copied from a real VM (`cat` the files once and save them in `testdata/`).

```go
func TestReadPSI(t *testing.T)          // "some avg10=1.23 avg60=0.50 avg300=0.10 total=123456\nfull … total=7890" → 123456µs, 7890µs
func TestReadPSISomeOnly(t *testing.T)  // only "some" line → FullTotal==0, no error
func TestReadCPUStat(t *testing.T)      // nr_periods 50, nr_throttled 5, throttled_usec 2500
func TestDirForPID(t *testing.T)        // "0::/system.slice/docker-abc.scope" → "/sys/fs/cgroup/system.slice/docker-abc.scope"
func TestDirForPIDCgroupV1(t *testing.T)// file with only "4:memory:/docker/abc" → error contains "cgroup v1"
func TestWholeDiskDeviceFromPartition(t *testing.T) // fake sysRoot: dev/block/259:1/partition exists, ../dev = "259:0" → "259:0"
func TestReadTCPCounters(t *testing.T)  // fixture snmp → OutSegs, RetransSegs match fixture values
func TestDiscoverSortsAndFills(t *testing.T) // fakeDocker returns app-02, app-01 → [app-01, app-02] with PID/IP/CgroupDir set
func TestDiscoverInspectError(t *testing.T)  // Inspect fails for one host → error names that host
```

- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/cgroup/... ./internal/netstat/... ./internal/fleet/...`. Expected: FAIL.
- [ ] **Step 3: Implement all readers and discovery.**
- [ ] **Step 4: Run the tests and confirm they pass.** Expected: PASS.
- [ ] **Step 5: Add an integration test** (`//go:build integration`), `TestDiscoverRealFleet`. It asserts 20 hosts and that `ReadPSI(h.CgroupDir+"/cpu.pressure")` and `ReadTCPCounters(fmt.Sprintf("/proc/%d/net/snmp", h.PID))` succeed for each. Run on the VM: `sudo go test -tags integration ./internal/fleet/...`. Expected: PASS.
- [ ] **Step 6: Commit.** `git commit -m "feat: cgroup/PSI/snmp readers and docker discovery"`

---

### Task 5: Chaos CLI (9 faults)

**Files:**
- Create: `internal/chaos/fault.go`, `internal/chaos/faults.go`, `internal/chaos/state.go`, `internal/chaos/runner.go`, `cmd/chaos/main.go`
- Test: `internal/chaos/faults_test.go`, `internal/chaos/state_test.go`

**Interfaces:**
- Consumes: `fleet.Host`, `fleet.Lookup`, `cgroup.WholeDiskDevice` (Task 4).
- Produces:
  - `type Runner interface { Run(ctx context.Context, name string, args ...string) error }` (real: `exec.CommandContext`), plus an `HTTP` poster `func(ctx context.Context, url string) error` for admin hooks (`http://<IP>:9090/admin/...`).
  - `type Env struct { Run Runner; Post func(ctx context.Context, url string) error; IODevice string; StateDir string }`
  - `type Fault interface { Name() string; Category() string; Apply(ctx context.Context, e Env, h fleet.Host) (saved map[string]string, err error); Revert(ctx context.Context, e Env, h fleet.Host, saved map[string]string) error }`
  - `func Registry() map[string]Fault`
  - `func ApplyWithState(ctx context.Context, e Env, f Fault, h fleet.Host) error`. It refuses with the error `"already applied"` if `<StateDir>/<host>__<fault>.json` exists. It writes the state file *before* returning success.
  - `func RevertAll(ctx context.Context, e Env) error`. It reverts every state file it finds, deletes each file after a successful revert, and keeps going on errors, returning them joined.
  - CLI: `chaos apply|revert -fault <name> -host <name>`, `chaos revert-all`, `chaos list`. The default `StateDir` is `/var/lib/graywatch/chaos`.

  Exact fault table (the values are pinned):

  | Name | Category | Apply | Revert |
  |---|---|---|---|
  | `net-loss` | network | `nsenter -t PID -n tc qdisc replace dev eth0 root netem loss 5%` | `nsenter -t PID -n tc qdisc del dev eth0 root` |
  | `net-delay` | network | `… netem delay 100ms 20ms` | same del |
  | `cpu-throttle` | cpu | save `cpu.max`; write `5000 100000` | write saved value |
  | `cpu-contention` | cpu | POST `/admin/burn?threads=2` | POST `/admin/burn?threads=0` |
  | `io-throttle` | disk | write `<IODevice> rbps=1048576 wbps=1048576 riops=50 wiops=20` to `io.max` (50 was above the 30 writes/s a host really does and had no effect) | write `<IODevice> rbps=max wbps=max riops=max wiops=max` |
  | `mem-pressure` | memory | save `memory.high`; write `anon` from `memory.stat` + 64MiB (`memory.current` counts page cache, which absorbed the fault on long-running hosts); POST `/admin/memhog?mb=300` | POST `/admin/memhog?mb=0`; write saved `memory.high` |
  | `dns-break` | app | save `ResolvConf` contents; overwrite with `nameserver 192.0.2.1\n` | write saved contents |
  | `pool-leak` | app | POST `/admin/leak?n=10` (holding 9 of 10 caused no failures) | POST `/admin/leak?n=0` |
  | `dep-slow` | fleet | `net-delay` with `delay 300ms` applied to host `dep` | same del |

- [ ] **Step 1: Write the failing tests** using a recording fake `Runner`/`Post` and a temp dir standing in for the cgroup dir.

```go
func TestNetLossCommand(t *testing.T)        // recorded == "nsenter -t 4242 -n tc qdisc replace dev eth0 root netem loss 5%"
func TestCPUThrottleSavesAndRestores(t *testing.T) // cpu.max "max 100000" → after Apply "5000 100000" → after Revert "max 100000"
func TestIOThrottleUsesDevice(t *testing.T)  // IODevice "259:0" → io.max contains "259:0 rbps=1048576"
func TestMemPressureSetsHighAboveCurrent(t *testing.T) // memory.current 100MiB → memory.high == 167772160; POST memhog?mb=300
func TestDNSBreakRestoresOriginal(t *testing.T)
func TestApplyTwiceFails(t *testing.T)       // second ApplyWithState → error "already applied"
func TestRevertAllUsesStateFiles(t *testing.T) // apply 2 faults on 2 hosts, new Env → RevertAll → both reverted, StateDir empty
func TestRevertAllContinuesOnError(t *testing.T)
func TestRegistryHasNineFaults(t *testing.T)
```

- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/chaos/...`. Expected: FAIL.
- [ ] **Step 3: Implement the faults, state handling and CLI.** The CLI fills `IODevice` with `cgroup.WholeDiskDevice("/sys", "deploy/data")` unless `-io-device` is passed.
- [ ] **Step 4: Run the tests and confirm they pass.** Expected: PASS.
- [ ] **Step 5: Verify each fault by hand on the VM, while loadgen runs.** Apply → check → revert:
  - `net-loss`: `nstat -az TcpRetransSegs` inside the netns climbs.
  - `cpu-throttle` / `cpu-contention`: `cpu.stat` `nr_throttled` climbs and `cpu.pressure` total climbs.
  - `io-throttle`: `io.pressure` total climbs. **If it doesn't, check `cat /sys/fs/cgroup/system.slice/cgroup.subtree_control` contains `io` before going further.**
  - `mem-pressure`: `memory.pressure` total climbs and the container is *not* OOM-killed.
  - `dns-break`, `pool-leak`: loadgen shows 500s with `X-Served-By` = target, while `show stat` keeps the target `UP` in shallow mode. **This is the gray failure. Screenshot it for the README.**
  - `dep-slow`: every host's latency rises.
  - `sudo chaos revert-all` leaves an empty `StateDir`.
- [ ] **Step 6: Commit.** `git commit -m "feat(chaos): nine reversible faults with crash-safe state"`

---

### Task 6: HAProxy runtime client and request-log listener

**Files:**
- Create: `internal/haproxy/runtime.go`, `internal/lblog/listener.go`, `internal/lblog/window.go`
- Test: `internal/haproxy/runtime_test.go` with `testdata/show_stat.csv`, `internal/lblog/window_test.go`, `internal/lblog/listener_test.go`

**Interfaces:**
- Produces:
  - `type ServerStat struct { Name, Status string; Scur int }`. Helper: `func (s ServerStat) Down() bool` returns `strings.HasPrefix(s.Status, "DOWN")`.
  - `type State string` with `Ready="ready"`, `Drain="drain"`, `Maint="maint"`
  - `type Client struct { Addr string; Timeout time.Duration }`
  - `func (c *Client) ShowStat(ctx context.Context, backend string) ([]ServerStat, error)`. It sends `show stat\n` on a new TCP connection, parses CSV whose header starts with `# `, and keeps rows where `pxname==backend` and `svname` is not `FRONTEND` or `BACKEND`.
  - `func (c *Client) SetState(ctx context.Context, backend, server string, s State) error`. It sends `set server <backend>/<server> state <s>\n`. Any non-blank reply is an error carrying that text.
  - `type Req struct { Server string; TrMs int; Status int; At time.Time }`
  - `func ParseLine(line string, at time.Time) (Req, bool)`. It finds the token `gw ` and parses `<server> <Tr> <ST>`. It returns false on malformed input or server `<NOSRV>`.
  - `type Window struct{…}`, `func NewWindow(retain time.Duration) *Window`, `func (w *Window) Add(r Req)` and `func (w *Window) Stats(server string, now time.Time, span time.Duration) (p90ms float64, errRate float64, n int)`. A `TrMs < 0` counts as 2000 ms for the percentile and as an error. Errors are `Status >= 500 || Status <= 0`. Entries older than `retain` are pruned.
  - `func Listen(ctx context.Context, addr string, w *Window) error` (UDP, writes into `w`).

- [ ] **Step 1: Write the failing tests.**

```go
func TestShowStatParsesServers(t *testing.T)   // fake TCP server replies testdata/show_stat.csv → 3 servers, app-02 Status "DOWN 1/2", Down()==true
func TestShowStatSkipsAggregateRows(t *testing.T)
func TestSetStateSendsExactCommand(t *testing.T) // captured == "set server be_app/app-03 state drain\n"
func TestSetStateErrorOnReply(t *testing.T)      // reply "No such server.\n" → err contains "No such server"
func TestParseLine(t *testing.T)  // "<134>Nov  2 10:00:00 haproxy[8]: gw app-03 12 200" → {app-03, 12, 200}
func TestParseLineRejects(t *testing.T) // "", "gw app-03 x 200", "gw <NOSRV> -1 503" → false
func TestWindowStats(t *testing.T) // 9×10ms/200 + 1×-1/504 within span → p90==10, errRate==0.1, n==10
func TestWindowSpanAndPrune(t *testing.T) // entries older than span excluded from Stats; older than retain gone from memory
func TestListenerReceivesUDP(t *testing.T) // send a datagram to Listen addr → Stats n==1
```

- [ ] **Step 2: Run them and confirm they fail.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and confirm they pass.** Expected: PASS.
- [ ] **Step 5: Live check.** Run a throwaway `main` that calls `Listen(":5140")` and prints `Stats` for `app-01` every second while loadgen runs. Expected: `n` ≈ 150 per 5 s (600 rps / 20 hosts × 5 s) and a sensible p90.
- [ ] **Step 6: Commit.** `git commit -m "feat(haproxy): runtime API client and request-log window"`

---

### Task 7: Collector

**Files:**
- Create: `internal/collect/collector.go`, `internal/collect/sources.go`
- Test: `internal/collect/collector_test.go`

**Interfaces:**
- Consumes: Task 4 readers, `fleet.Host`, and `lblog.Window.Stats`.
- Produces:
  - `type Signal string` with constants `SigLatP90="lat_p90_ms"`, `SigErrRate="err_rate"`, `SigCPUPSI="cpu_psi"`, `SigIOPSI="io_psi"`, `SigMemPSI="mem_psi"`, `SigThrottle="cpu_throttle"`, `SigRetrans="tcp_retrans"`
  - `type Sample struct { Host string; At time.Time; Values map[Signal]float64 }`. A missing key means no data, which is different from 0.
  - `type KernelSnap struct { CPU, IO, Mem cgroup.PSI; CPUStat cgroup.CPUStat; TCP netstat.TCPCounters }`
  - `type KernelSource interface { Read(h fleet.Host) (KernelSnap, error) }` (real: `FSKernelSource{}` reading `cpu.pressure`, `io.pressure`, `memory.pressure`, `cpu.stat`, `/proc/<pid>/net/snmp`)
  - `type ClientSource interface { Stats(server string, now time.Time, span time.Duration) (p90ms, errRate float64, n int) }` (satisfied by `*lblog.Window`)
  - `func New(k KernelSource, c ClientSource, now func() time.Time) *Collector`
  - `func (c *Collector) SetHosts(hs []fleet.Host)`. If a host's PID changed, its previous snapshot is dropped.
  - `func (c *Collector) Tick() []Sample`. The rates computed for each host, with `dt` the wall time since the previous snapshot, are:
    - `cpu_psi = ΔCPU.SomeTotal/dt`, `io_psi = ΔIO.SomeTotal/dt`, `mem_psi = ΔMem.SomeTotal/dt`
    - `cpu_throttle = ΔThrottledUsec/dt`
    - `tcp_retrans = ΔRetransSegs/ΔOutSegs`, only if `ΔOutSegs ≥ 20`
    - `lat_p90_ms`, `err_rate` from `Stats(name, now, 5s)`, only if `n ≥ 20`
    - On the first tick for a host, a read error, or any counter decrease, that host's kernel signals are omitted for that tick. Client signals still come through.

- [ ] **Step 1: Write the failing tests** with fake sources and a controllable clock.

```go
func TestFirstTickHasNoKernelRates(t *testing.T)
func TestPSIRate(t *testing.T)            // CPU.SomeTotal +300ms over 1s → cpu_psi == 0.3
func TestThrottleRate(t *testing.T)       // ThrottledUsec +500000 over 1s → 0.5
func TestRetransNeedsMinSegs(t *testing.T)// ΔOut 10 → no key; ΔOut 200, ΔRetrans 10 → 0.05
func TestClientSignalsNeedMinRequests(t *testing.T) // n=19 → no keys; n=20 → both keys
func TestCounterResetSkipsSample(t *testing.T)      // SomeTotal decreases → no cpu_psi this tick, value present next tick
func TestPIDChangeDropsSnapshot(t *testing.T)       // SetHosts with new PID → next tick has no kernel rates for that host
func TestReadErrorOmitsOnlyThatHost(t *testing.T)
```

- [ ] **Step 2: Run them and confirm they fail.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and confirm they pass.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(collect): per-tick signal rates with reset handling"`

---

### Task 8: Detector

**Files:**
- Create: `internal/detect/robust.go`, `internal/detect/detector.go`, `internal/detect/config.go`
- Test: `internal/detect/robust_test.go`, `internal/detect/detector_test.go`

**Interfaces:**
- Consumes: `collect.Sample`, `collect.Signal*`.
- Produces:
  - `func Median(xs []float64) float64` and `func RobustZ(xs []float64, i int, minMAD float64) float64`
  - `type Config struct { Z float64; Window, Need, MinHosts int; MinMAD, MinDelta map[collect.Signal]float64; Category map[collect.Signal]string; FleetLatencyMs, FleetErrRate float64 }` and `func DefaultConfig() Config`, with values exactly as in Global Constraints.
  - `type HostState string` with `Healthy`, `Suspect`
  - `type Verdict struct { Host string; State HostState; Signals []collect.Signal; Category string; Score float64; Since time.Time }`. `Score` is the max z over flagged signals in the latest hit tick.
  - `type FleetAlert struct { Reason string; MedianLatMs, MedianErr float64 }`
  - `type Observation struct { At time.Time; Verdicts []Verdict; Fleet *FleetAlert }` (verdicts sorted by Host)
  - `func New(cfg Config) *Detector` and `func (d *Detector) Observe(samples []collect.Sample, drained map[string]bool) Observation`

  Algorithm (not determined by the signatures):

```
for each signal s:
    peers = values of s from samples that have s AND host not drained
    if len(peers) < MinHosts: skip s
    m = Median(peers); mad = Median(|v − m|); madEff = max(mad, MinMAD[s])
    for every sample h having s (drained hosts included):
        z = 0.6745 · (x_h − m) / madEff                # one-sided: only "worse" counts
        flagged(h,s) = z > Z  &&  (x_h − m) > MinDelta[s]
hit(h) = any flagged(h,·)
history(h) = last Window hit booleans
Healthy → Suspect when count(history) ≥ Need        (Since = now)
Suspect → Healthy when count(history) == 0
Category = Category[s*] where s* = flagged non-"app" signal with the highest z; else "app"
Fleet alert if Median(lat_p90_ms of non-drained) > FleetLatencyMs or Median(err_rate) > FleetErrRate
```

- [ ] **Step 1: Write the failing tests.**

```go
func TestRobustZ_Basic(t *testing.T)         // [9,10,10,10,11,50], i=5, minMAD 0 → z ≈ 53.96 (±0.01)
func TestRobustZ_MADZeroUsesFloor(t *testing.T) // [0,0,0,0,0.03], i=4, minMAD 0.005 → z ≈ 4.047, !IsInf, !IsNaN
func TestSingleSpikeStaysHealthy(t *testing.T)  // 20 hosts, app-03 io_psi 0.5 for 1 tick of 5 → Healthy
func TestSustainedOutlierBecomesSuspect(t *testing.T) // 3 consecutive ticks → Suspect on tick 3, Signals ⊇ {io_psi}, Category "disk"
func TestRecoversAfterQuietWindow(t *testing.T)       // then 5 clean ticks → Healthy
func TestTooFewHostsSkipsSignal(t *testing.T)         // only 4 hosts report cpu_psi → never flagged on it
func TestSmallAbsoluteDeltaIgnored(t *testing.T)      // lat 12ms vs 10ms fleet → not flagged (delta < 50)
func TestSymptomOnlyIsApp(t *testing.T)               // only err_rate flagged → Category "app"
func TestMissingClientSignalsNotEvidence(t *testing.T)// drained app-05 has only kernel keys, all normal → Healthy; drained host excluded from peer median
func TestFleetWideShiftRaisesFleetAlertNotSuspects(t *testing.T) // all 20 at lat 400ms → no Suspect, Fleet != nil
```

- [ ] **Step 2: Run them and confirm they fail.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and confirm they pass.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(detect): peer-relative robust z detector with debounce"`

---

### Task 9: Remediation planner and the `graywatchd` daemon

**Files:**
- Create: `internal/remediate/planner.go`, `internal/remediate/actuator.go`, `internal/events/events.go`, `internal/metrics/metrics.go`, `cmd/graywatchd/main.go`
- Test: `internal/remediate/planner_test.go`, `internal/remediate/actuator_test.go`, `internal/events/events_test.go`

**Interfaces:**
- Consumes: `detect.Observation`, `haproxy.Client`, `collect.Collector`, `fleet.Discover`, `lblog.Listen`.
- Produces:
  - `type Policy struct { MaxDrainFrac, FleetWideFrac float64; Cooldown, MinActionGap, FlapWindow time.Duration; MaxDrainsPerWindow int; DryRun bool }` and `func DefaultPolicy() Policy`
  - `type ActionKind string` with `Drain`, `Restore`, `AlertFleetWide`, `AlertCapReached`, `AlertFlapping`
  - `type Action struct { Kind ActionKind; Host, Reason string; DryRun bool }`
  - `func NewPlanner(p Policy, fleetSize int, alreadyDrained []string) *Planner`. Servers already in DRAIN/MAINT at startup count toward the cap.
  - `func (pl *Planner) Plan(obs detect.Observation) []Action`. This is pure: it only reads planner state.
  - `func (pl *Planner) Commit(a Action, at time.Time, err error)`. On success it updates the drained set, drain history and last-action time. It does nothing when `err != nil` or `a.DryRun`.
  - `func (pl *Planner) Drained() map[string]bool`
  - `type Actuator interface { Drain(ctx context.Context, host string) error; Restore(ctx context.Context, host string) error }` with `HAProxyActuator{C *haproxy.Client; Backend string}`. Drain is `SetState(Drain)`, then polls `ShowStat` until `Scur==0` or 5 s, then `SetState(Maint)`. Restore is `SetState(Ready)`.
  - `type Event struct { TS time.Time; Type, Host, Category, Reason string; Signals []string; Score float64; DryRun bool }`. Types: `suspect`, `healthy`, `drain`, `restore`, `fleet_alert`, `cap_reached`, `flapping`, `action_error`.
  - `func NewWriter(w io.Writer) *Writer`, `func (w *Writer) Emit(e Event) error` (one JSON object per line) and `func ReadAll(r io.Reader) ([]Event, error)`.
  - Metrics: `graywatch_signal{host,signal}`, `graywatch_robust_z{host,signal}`, `graywatch_host_suspect{host}` (0/1), `graywatch_actions_total{kind}`.
  - `graywatchd` flags: `-haproxy 127.0.0.1:9999 -backend be_app -log-listen :5140 -interval 1s -metrics :9100 -events events.jsonl -dry-run`. The loop is: rediscover hosts every 30 s → `Tick` → `Observe(samples, planner.Drained())` → emit `suspect`/`healthy` on state transitions → `Plan` → execute each action → `Commit` → emit.

  Planning rules, in order:
  1. If `obs.Fleet != nil` or `#Suspect > FleetWideFrac·N`, emit `AlertFleetWide` and no drains.
  2. If `now − lastAction < MinActionGap`, emit nothing.
  3. Restore: a drained host that is `Healthy` and was drained ≥ `Cooldown` ago, unless it has `MaxDrainsPerWindow` drains in the last `FlapWindow`, in which case emit `AlertFlapping` once.
  4. Drain: the Suspect, not-yet-drained host with the highest `Score`, if `len(drained) < cap`. If it is blocked by the cap, emit `AlertCapReached`.
  5. At most one Drain/Restore per Plan call.

- [ ] **Step 1: Write the failing tests.**

```go
func TestDrainsSuspect(t *testing.T)                 // N=20, app-03 Suspect → [Drain app-03]
func TestDrainPicksHighestScore(t *testing.T)
func TestCapsConcurrentDrains(t *testing.T)          // drained {a,b} (cap 2), new Suspect → AlertCapReached, no Drain
func TestFleetWideSuspicionBlocksDrains(t *testing.T)// 7 Suspects of 20 → only AlertFleetWide
func TestFleetAlertBlocksDrains(t *testing.T)
func TestMinActionGap(t *testing.T)                  // Commit drain at t; Plan at t+5s → nil; t+10s → Drain
func TestRestoreAfterCooldown(t *testing.T)          // drained at t, Healthy at t+59s → nil; t+61s → Restore
func TestNoRestoreWhileSuspect(t *testing.T)
func TestFlapProtection(t *testing.T)                // 3 drains in 10m → no Restore, one AlertFlapping
func TestPreexistingDrainsCountTowardCap(t *testing.T)
func TestActuatorFailureNotCountedAsDrained(t *testing.T) // Commit(Drain, err) → Drained() lacks host; next Plan → Drain again
func TestDryRunNeverCallsActuator(t *testing.T)      // executor with DryRun policy + recording actuator → 0 calls, events DryRun=true
func TestHAProxyDrainThenMaint(t *testing.T)         // fake runtime server: commands == [drain, show stat…, maint]
func TestEventsRoundTrip(t *testing.T)
```

- [ ] **Step 2: Run them and confirm they fail.** Expected: FAIL.
- [ ] **Step 3: Implement the planner, actuator, events, metrics and `graywatchd` main.**
- [ ] **Step 4: Run the tests and confirm they pass.** Run `go test ./...`. Expected: PASS.
- [ ] **Step 5: End-to-end on the VM.**
  - With loadgen at 600 rps, start `sudo graywatchd -events /tmp/ev.jsonl`.
  - Run `sudo chaos apply -fault pool-leak -host app-07`. Expected within about 10 s: `suspect app-07` and then `drain app-07` in `/tmp/ev.jsonl`, `show stat` shows app-07 `MAINT`, and the loadgen error rate falls back toward baseline.
  - Revert. Expected: `restore app-07` after the cooldown.
  - Run `chaos apply -fault dep-slow -host dep`. Expected: `fleet_alert` and **no** `drain`.
  - Kill graywatchd while app-07 is drained, then restart it. Expected: app-07 still counts toward the cap.
- [ ] **Step 6: Commit.** `git commit -m "feat(graywatchd): guarded auto-drain daemon with events and metrics"`

---

### Task 10: Benchmark harness

**Files:**
- Create: `internal/bench/scenario.go`, `internal/bench/score.go`, `internal/bench/orchestrate.go`, `cmd/bench/main.go`, `bench/scenarios/{none,net-loss,net-delay,cpu-throttle,cpu-contention,io-throttle,mem-pressure,dns-break,pool-leak,dep-slow}.yaml`
- Test: `internal/bench/scenario_test.go`, `internal/bench/score_test.go`

**Interfaces:**
- Consumes: `loadgen.Run/Result/Percentile`, `chaos.ApplyWithState/RevertAll/Registry`, `haproxy.Client.ShowStat/SetState`, `events.ReadAll`, `fleetgen.Render`.
- Produces:
  - `type Scenario struct { Name, Fault, ExpectedCategory, Target string; Baseline, FaultDur, Cooldown time.Duration }` (`Target`: `random` | `dep` | `none`) and `func LoadScenarios(dir string) ([]Scenario, error)`
  - `type HCObs struct { At time.Time; Server string; Down bool }`
  - `type TrialResult struct { Scenario, Mode, Health, Target string; Trial int; HCDetected bool; HCTTD time.Duration; GWDetected bool; GWTTD time.Duration; GWCategory string; CategoryOK bool; FalsePosHosts, Drains int; FleetAlert bool; P50, P99 time.Duration; ErrRate float64; BaselineP99 time.Duration }`
  - `func Score(sc Scenario, target string, tBase, t0, t1 time.Time, evs []events.Event, hc []HCObs, reqs []loadgen.Result) TrialResult`. This is pure:
    - GW detection: the first `suspect` on the target in `[t0, t1]`.
    - HC detection: the first `Down` on the target in `[t0, t1]`.
    - False positives: distinct non-target hosts with a `suspect` in `[tBase, t1+Cooldown]`, plus the target if it had a `suspect` in `[tBase, t0)`.
    - p50/p99/err from `reqs` scheduled in `[t0, t1]`; BaselineP99 from `[tBase, t0)`.
    - For `Target=="dep"`, `Drains` counts all hosts.
  - `func WriteTrialsCSV(w io.Writer, rs []TrialResult) error`
  - CLI: `bench run -scenarios bench/scenarios -trials 3 -configs shallow:detect,shallow:remediate,deep:detect -seed 42 -out results/<date> [-only pool-leak,net-loss]`
  - Per config: render the fleet with that health mode → `docker compose up -d --force-recreate` → wait until 20 `UP` → start `graywatchd` (`-dry-run` when mode is `detect`) → start loadgen in-process → 60 s warmup. Per trial: pick the target (seeded) → wait `Baseline` → `ApplyWithState` (t0) → wait `FaultDur` → revert (t1) → wait `Cooldown` → set every be_app server to `ready` → score → append CSV.
  - Scenario timings: `Baseline: 30s, FaultDur: 90s, Cooldown: 30s`. The `none` scenario has `FaultDur: 600s` with no fault and is used for the false-positive rate per host-hour.

- [ ] **Step 1: Write the failing tests.**

```go
func TestLoadScenarios(t *testing.T)          // 10 files parse; pool-leak ExpectedCategory "app"; dep-slow Target "dep"
func TestScoreDetectionAndTTD(t *testing.T)   // suspect app-07 at t0+6s → GWDetected, GWTTD==6s
func TestScoreIgnoresDetectionBeforeFault(t *testing.T) // suspect app-07 at t0−10s → GWDetected false, FalsePosHosts==1
func TestScoreHealthCheck(t *testing.T)       // HCObs app-07 Down at t0+8s → HCDetected, HCTTD==8s
func TestScoreFalsePositives(t *testing.T)    // suspects on app-02, app-02, app-11 → FalsePosHosts==2
func TestScoreLatencyWindow(t *testing.T)     // only reqs scheduled in [t0,t1] feed P99
func TestScoreCategory(t *testing.T)          // first suspect Category "disk" vs expected "disk" → CategoryOK
func TestScoreDepScenario(t *testing.T)       // fleet_alert present, 0 drains → FleetAlert true, Drains 0
```

- [ ] **Step 2: Run them and confirm they fail.** Expected: FAIL.
- [ ] **Step 3: Implement the scenarios, `Score`, the orchestrator and the CLI.** On SIGINT/SIGTERM, cancel the context, then run `chaos.RevertAll`, set all servers to `ready`, stop `graywatchd` and flush the CSV of finished trials, in that order.
- [ ] **Step 4: Run the tests and confirm they pass.** Expected: PASS.
- [ ] **Step 5: Short dry run on the VM.** Run `sudo bench run -trials 1 -configs shallow:detect -scenarios bench/scenarios -only pool-leak,net-loss`. Expected: 2 CSV rows with sensible values. Then Ctrl-C a second run mid-fault and confirm `ls /var/lib/graywatch/chaos` is empty and every server in `show stat` is `UP`.
- [ ] **Step 6: Full run overnight.** That is 3 configs × 9 faults × 3 trials × ~2.5 min + the `none` baseline, roughly 4 h. Run it with `nohup`.
- [ ] **Step 7: Commit.** `git commit -m "feat(bench): fault-injection benchmark with scoring"`

---

### Task 11: Report, dashboard and README

**Files:**
- Create: `internal/bench/report.go`, `deploy/prometheus.yml`, `deploy/grafana/provisioning/{datasources,dashboards}/*.yml`, `deploy/grafana/graywatch.json`, `docs/RESULTS.md` (generated), `README.md`
- Modify: `internal/fleetgen/templates/compose.yml.tmpl` (add the `prometheus` and `grafana` services; Prometheus scrapes `host.docker.internal:9100`)
- Test: `internal/bench/report_test.go`

**Interfaces:**
- Consumes: `TrialResult` CSV.
- Produces:
  - `func Summarize(rs []TrialResult) []Row` and `func RenderMarkdown(rows []Row, fpPerHostHour float64) string`
  - One row per fault with these columns: `Fault | HC caught (shallow) | HC caught (deep) | Graywatch caught | Median TTD | Category right | False-positive hosts | p99 detect-only | p99 remediate`. Rates are formatted as `k/n`.
  - CLI: `bench report results/<date>` writes `docs/RESULTS.md`.
  - Dashboard panels: a z-score heatmap (host × time), suspect state timeline, actions annotations from `graywatch_actions_total`, and client p90 by host.

- [ ] **Step 1: Write the failing tests.**

```go
func TestSummarizeRates(t *testing.T)      // 3 trials, GW detected 2 → "2/3"; median TTD of detected only
func TestRenderMarkdownColumns(t *testing.T) // header row exactly matches the column list above
```

- [ ] **Step 2: Run them and confirm they fail, then implement, then run them and confirm they pass.**
- [ ] **Step 3: Generate the report.** Run `bench report results/<date>`. Expected: `docs/RESULTS.md` with 9 fault rows plus the baseline FP rate.
- [ ] **Step 4: Bring up the dashboard.** Bring up Prometheus/Grafana, rerun one `pool-leak` trial, and screenshot the heatmap showing app-07 lighting up before the drain.
- [ ] **Step 5: Write the README in your own words.** Cover:
  - the problem, using one sentence about your internship connection-pool incident
  - the architecture diagram (Mermaid)
  - how to run (`vm-setup` → `fleetgen` → `compose up` → `graywatchd` → `bench`)
  - the results table
  - a **Limitations** section: containers share one kernel, so this simulates a fleet; network delay is diagnosed as "app" without RTT data; HAProxy logs are the only client view
  - what you'd do next (Task 12)
- [ ] **Step 6: Commit.** `git commit -m "docs: results, dashboard, README"`

---

### Task 12 (stretch, only if Weeks 1–3 are done): eBPF signals

**Files:**
- Create: `internal/ebpf/runqlat.bpf.c`, `internal/ebpf/runqlat.go` (generated via `bpf2go`), `internal/ebpf/source.go`
- Modify: `internal/collect/collector.go` to add `SigRunqP99="runq_p99_us"`, `internal/detect/config.go` (MinMAD 50, MinDelta 3000, Category cpu; 2000 flagged healthy hosts)

**Interfaces:**
- Produces: `type RunqSource interface { P99ByCgroup() (map[uint64]float64, error) }`. Collector maps cgroup id → host via the inode of `CgroupDir` (`stat` inode == cgroup id on cgroup v2).

Notes that decide the design:
- Use `tp_btf/sched_wakeup` to store the timestamp per pid, then `tp_btf/sched_switch` to compute the delta for `next` and key it by `BPF_CORE_READ(next, cgroups, dfl_cgrp, kn, id)`. Read `next`'s cgroup, **not** `bpf_get_current_cgroup_id()`, because in `sched_switch` "current" is still the previous task.
- If you add TCP retransmits later, attribute them by the socket's cgroup (`sk->sk_cgrp_data`), not the current task, because retransmits fire from timer softirq context.

- [ ] **Step 1:** Write a Go test for the per-cgroup log2-histogram → p99 conversion. This is pure: given bucket counts it returns the p99 bucket upper bound.
- [ ] **Step 2:** Implement and load the program, then add an integration test where `cpu-contention` on app-03 raises its `runq_p99_us` above the fleet median.
- [ ] **Step 3:** Rerun the `cpu-*` scenarios and add a column to RESULTS.md.
- [ ] **Step 4:** Commit.
