# Quickstart timing

Measured on 2026-11-16 on the machine listed as "Tested on" in the README
(macOS, arm64), then projected to a 100 Mbit/s
link (12.5 MB/s). Downlink D = 19.9 MB/s, the mean of two `curl -L` downloads of
the export capped at 20 s (22.65 and 17.07 MB/s). The projection swaps the time
the ingest spends reading the file at D for the time at 12.5 MB/s.

| Step | Measured here | At 100 Mbit/s | How |
| --- | --- | --- | --- |
| `git clone` | not measured over the network | 0.12 s | transfer term only: 1.47 MiB packed repo at 12.5 MB/s; a local copy took 0.3 s; GitHub latency not measured |
| Image pull | not run | 63 s (arm64), 83 s (amd64) | config and layer bytes from `docker manifest inspect -v` (789,335,885 and 1,040,156,347) divided by 12.5 MB/s |
| `make up` until healthy | 6.95 s | 12 s | `docker run` with the same image, env and ulimits as `docker-compose.yml`, plus 5 s for the compose health-check interval |
| `make ingest`, cold Go caches | 59 s | 89 s | 58.95 s wall with empty build and module caches, minus 51.1 s of download at D, plus 81.2 s at 12.5 MB/s (about 1.01e9 bytes read; the figure varies by run); 131 s if the whole export is read (1,536,486,783 bytes) |
| `make run` until "listening" | 1.63 s | 1.63 s | measured |
| First `curl` | 0.16 s | 0.16 s | measured |
| Total | | about 166 s (arm64), 186 s (amd64) | 208 s and 228 s with the 131 s ingest |

The target is under 5 minutes. A slower toolchain download, an older Go or a
slower disk adds to this. The ingest process peaked at 27.0 MiB RSS (prebuilt
binary) and the container at 1.06 GiB (30 `docker stats` samples about 2 s
apart, full ingest); this differs from the 818-883 MiB of an earlier
integration-test run, which loads a small fixture.
