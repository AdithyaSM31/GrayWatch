## How the numbers were made

One Azure `Standard_D4ds_v4` VM (4 vCPU, 16 GiB, local SSD) ran Ubuntu 24.04, 20 app containers, HAProxy, Postgres, the load generator and `graywatchd`. Load was 600 requests a second. Each fault ran for 90 s on one host, picked with seed 42, three times in each of three configs: `shallow:detect`, `shallow:remediate` and `deep:detect`. `dep-slow` hits the shared dependency, so every host is affected.

Raw results are in `docs/data/trials.csv`, and the eBPF run is in `docs/data/ebpf/trials.csv`.

## How to read the tables

- HC caught (shallow) counts `shallow:detect` trials only. In a remediate run Graywatch drains first, and a server in maintenance can't read as down, so those trials say nothing about the health check.
- Graywatch caught counts all nine trials of each fault. For `dep-slow` it means a fleet alert, since no single host is at fault. It raised no drains in any trial, which is the correct behaviour. Its category shows `n/a` because a fleet alert is what catching it means, so that score could not fail.
- p99 detect-only is fleet-wide, not the target's. One failing host in 20 is 5% of traffic, so a p99 near a timeout means the host was still failing requests. A fault that only slows one host by less than that can barely move it.
- Errors are fleet-wide too. 5% is about one host failing everything.

## What these results don't show

- Three trials per config is few. These are direct comparisons on one machine, not estimates with error bars. Zero false positives in 11 host-hours still fits a true rate of up to about 0.27 per host-hour.
- Reverting `io-throttle` on an undrained host slows the whole fleet. It raised a fleet alert in all six undrained trials and none of the drained ones, and caused the one false positive in the fault trials. The likely cause is 90 s of blocked writes flushing at once through the shared ext4 journal, though that isn't proven.
- All the containers share one kernel and disk, so this simulates a fleet rather than being one.
- `mem-pressure` isn't gray at this strength. It nearly stalls the host, so even the shallow check catches it, in about 16 s against Graywatch's 3 s.
- Category accuracy is 59 of 72, leaving out `dep-slow`. `net-delay` accounts for 9 misses: delay alone causes no retransmits, and without round-trip data the slow host looks like a slow application. The other 4, in `cpu-contention` and `net-loss`, are timing. The category comes from the tick that crosses the debounce, and on those ticks latency happened to be the only signal flagged. Graywatch still detected and drained the host in all of them.
- Deep checks beat Graywatch's drains on errors for `pool-leak` and `dns-break`, at about 0.45% against 0.79%, even though Graywatch reacts 4 to 7 s sooner. A drained host gets no traffic, so it reads as healthy, is restored after the 60 s cooldown while the fault is still on, and fails requests for about 10 s until it is drained again. A flap guard limits how often this repeats. Graywatch's clearer advantage is on faults that deep checks can't see, such as `io-throttle`, `net-loss` and `cpu-contention`.
- The false positive rate is for a warmed-up fleet. The benchmark sends 60 s of traffic before its first trial, and a host's first requests are slow, so suspects can appear when traffic first starts. The 20 s startup grace stops those from causing drains, but they aren't counted here.

## What the run queue signal adds

Nothing measurable to detection time. Three flagged ticks out of five is the fastest a host can become suspect, and the existing signals already reach that on these faults. It did show up as evidence, in 4 of the 6 suspect events.

It is off by default (`-ebpf`). It traces every context switch, about 100,000 a second on this VM, though CPU idle and p99 stayed within normal variation in four comparisons of about a minute each, two with it and two without. Its `MinDelta` is 3000 us. The plan's 2000 us falsely flagged 16 of 20 healthy hosts, because healthy p99 is about 2048 us and the log2 histogram makes the next step up exactly 2048 us higher.

The eBPF run is a single config, so it is a smaller sample than the main table.
