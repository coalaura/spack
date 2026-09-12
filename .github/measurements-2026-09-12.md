# Corpus measurements, 2026-09-12

All results are single runs on revision `e316083efdf242fd7a54b11e83fdbe37745f5712` with a modified worktree containing the measurement and diagnostic test changes. Corpora remained outside Git.

Environment: Go `go1.27.1`, `windows/amd64`, AMD Ryzen 9 9950X3D 16-Core Processor, `GOMAXPROCS=32`, `Pointer32`, `PackOptions.DisableGC=false`, default `GOGC=100`, no `GOMEMLIMIT`.

The gap is exactly `current blob - lower bound`. The percentage is `100 * gap / current blob`; it is the maximum possible remaining saving, not necessarily an attainable saving.

| Corpus file | SHA-256 | Inputs | Roots | Current blob bytes | Lower-bound bytes | Gap bytes | Gap |
|---|---|---:|---:|---:|---:|---:|---:|
| `commoncrawl-cc-main-2026-34-url-occurrences-50m.spc` | `8211781c1be4872600d5de07c4b53d49280ef5d7781123556a8cc71d5503df04` | 50,000,000 | 47,953,063 | 3,429,845,090 | 3,429,840,415 | 4,675 | 0.000136% |
| `joaat-sh-gta-v-extracted-unique-81m.spc` | `fb282b81a78be350988314cc32bab1431d5eb052b1f7fc8741fa6df72c731388` | 81,845,115 | 55,448,624 | 1,122,691,521 | 963,186,667 | 159,504,854 | 14.207362% |
| `osm-geofabrik-2026-09-05-tag-value-occurrences-balanced-50m.spc` | `7b19c4f4716e9943b43ed923306e955bb54563f6f7cd57efd28f374fb8e523cd` | 50,000,000 | 3,858,803 | 94,622,364 | 82,860,302 | 11,762,062 | 12.430531% |
| `wikidata-20260831-label-alias-occurrences-50m.spc` | `b56d86e3cf351e2689ab4d8e6e009638c3c34043875c189bf8d8178764f8af6c` | 50,000,000 | 10,875,762 | 334,585,518 | 294,320,602 | 40,264,916 | 12.034267% |

## Timing-only processes

Corpus loading and hashing are outside every duration. Ordinary time covers the full `Pack` call with diagnostics disabled. Diagnostic time covers the full `PackWithBlobSizeBound` call. Bound time is measured inside the bound calculation and must not be subtracted to derive ordinary Pack time.

| Corpus | Ordinary Pack | Diagnostic-enabled Pack | Bound calculation within diagnostic call |
|---|---:|---:|---:|
| Common Crawl | 22.935 s | 115.247 s | 92.624 s |
| joaat.sh | 31.434 s | 136.434 s | 106.892 s |
| OpenStreetMap | 2.193 s | 7.288 s | 5.009 s |
| Wikidata | 5.347 s | 19.958 s | 14.677 s |

## Heap-sampling processes

These are separate processes from the timing-only runs. The monitor samples `runtime.MemStats.Alloc` every 1 ms during the Pack call. Values are sampled Go heap allocation, not RSS or total process memory. Sampling can miss transient peaks and differences between rows are not exact allocation costs.

| Corpus | Operation | Pack call in sampling run | Pre-call Go heap allocation | Sampled peak Go heap allocation |
|---|---|---:|---:|---:|
| Common Crawl | ordinary | 24.799 s | 4,668.82 MiB | 8,824.92 MiB |
| Common Crawl | diagnostic-enabled | 134.383 s | 4,668.82 MiB | 8,824.94 MiB |
| joaat.sh | ordinary | 38.923 s | 3,420.30 MiB | 8,205.95 MiB |
| joaat.sh | diagnostic-enabled | 174.646 s | 3,420.30 MiB | 8,120.38 MiB |
| OpenStreetMap | ordinary | 2.712 s | 1,540.64 MiB | 2,282.75 MiB |
| OpenStreetMap | diagnostic-enabled | 7.765 s | 1,540.63 MiB | 2,363.15 MiB |
| Wikidata | ordinary | 6.563 s | 2,047.91 MiB | 3,210.48 MiB |
| Wikidata | diagnostic-enabled | 24.449 s | 2,047.91 MiB | 3,428.52 MiB |

The commands and environment switches used for these four fresh-process modes are documented in the main README.
