## How the numbers were made

One Azure `Standard_D4ds_v4` VM (4 vCPU, 16 GiB, local SSD) ran Ubuntu 24.04, 20 app containers, HAProxy, Postgres, the load generator and `graywatchd`. Load was 600 requests a second. Each fault ran for 90 s on one host, picked with seed 42, three times in each of three configs: `shallow:detect`, `shallow:remediate` and `deep:detect`. `dep-slow` hits the shared dependency, so every host is affected.

Raw results are in `docs/data/trials.csv`.

## How to read the tables

- HC caught (shallow) counts `shallow:detect` trials only. In a remediate run Graywatch drains first, and a server in maintenance can't read as down, so those trials say nothing about the health check.
- Graywatch caught counts all nine trials of each fault. For `dep-slow` it means a fleet alert, since no single host is at fault. It raised no drains in any trial, which is the correct behaviour. Its category shows `n/a` because a fleet alert is what catching it means, so that score could not fail.
- p99 detect-only is fleet-wide, not the target's. One failing host in 20 is 5% of traffic, so a p99 near a timeout means the host was still failing requests. A fault that only slows one host by less than that can barely move it.
- Errors are fleet-wide too. 5% is about one host failing everything.

## What these results don't show

- Three trials per config is few. These are direct comparisons on one machine, not estimates with error bars. Zero false positives in 11 host-hours still fits a true rate of up to about 0.27 per host-hour.
- `mem-pressure` ran separately. Its first results were invalid: the fault's limit counted page cache, so on hosts that had run for a while it did nothing. After the fix, it was rerun alone on a fresh fleet, on different hosts. Those are the rows here.
- `mem-pressure` isn't gray at this strength. It nearly stalls the host, so even the shallow check catches it, in about 13 s against Graywatch's 3 s.
- `net-delay` scores 0/9 on category. Delay alone causes no retransmits, and without round-trip data the slow host looks like a slow application. Graywatch still detects and drains it.
- Deep checks beat Graywatch's drains on errors for `pool-leak` and `dns-break`, at about 0.48% against 0.73%, even though Graywatch reacts sooner. A drained host gets no traffic, so it reads as healthy, is restored after the 60 s cooldown while the fault is still on, and fails requests for about 10 s until it is drained again. A flap guard limits how often this repeats. Graywatch's clearer advantage is on faults that deep checks can't see, such as `io-throttle`, `net-loss` and `cpu-contention`.
- Reverting `io-throttle` on an undrained host slows the whole fleet. It raised a fleet alert in all six undrained trials and none of the drained ones, and caused the one false positive in the fault trials. The likely cause is 90 s of blocked writes flushing at once through the shared ext4 journal, though that isn't proven.
- All the containers share one kernel and disk, so this simulates a fleet rather than being one.
- The false positive rate is for a warmed-up fleet. The benchmark sends 60 s of traffic before its first trial, and a host's first requests are slow, so suspects can appear when traffic first starts. Those runs had no startup grace period, which was added afterward so that these suspects can't cause drains. One `pool-leak` trial rerun with it matched these results: detected in 3.0 s, drained twice, 0.78% errors.
