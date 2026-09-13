# Cyclic-cover research, 2026-09-13

These are opt-in research results from the exact EHOG cyclic-cover evaluator and constructor in `cyclic_cover_research_test.go`. Corpora remained local and are not part of the repository.

Environment: Go `go1.27.1`, `windows/amd64`, AMD Ryzen 9 9950X3D 16-Core Processor, `GOMAXPROCS=32`, `Pointer32`, default GC settings. Corpus loading and hashing are outside the reported durations.

## Certificate

The evaluator constructs every distinct proper overlap in the containment- reduced roots, including proper self-overlaps, plus the empty node. It computes both exact EHOG parent relationships and runs Algorithm 2 from Cazaux, Juhel and Rivals, "Practical lower and upper bounds for the Shortest Linear Superstring," SEA 2018. The certified cyclic-cover norm is root bytes minus the algorithm's overlap savings.

This is a lower bound for the linear shortest-superstring problem because every linear superstring induces a cyclic cover no longer than itself. In contrast, a feasible cover or an optimum over a pruned overlap graph would only be an upper bound on the unrestricted cyclic-cover optimum and is not used as a certificate.

Small-instance tests compare overlap savings with an independent exact assignment DP and compare the norm with an independent exact linear shortest-superstring DP. Tests cover arbitrary bytes, duplicates, containment, periodic strings, proper self-overlaps, 255-byte strings and root sets above the ordinary ten-root refinement limit.

## Results

The existing independent bounds are from `measurements-2026-09-12.md`.

| Corpus | SHA-256 | Current blob | Existing bound | Cyclic norm | Strongest bound | Remaining gap | Gap |
|---|---|---:|---:|---:|---:|---:|---:|
| Common Crawl | `8211781c1be4872600d5de07c4b53d49280ef5d7781123556a8cc71d5503df04` | 3,429,845,090 | 3,429,840,415 | 3,429,835,522 | 3,429,840,415 | 4,675 | 0.000136% |
| GTA V | `fb282b81a78be350988314cc32bab1431d5eb052b1f7fc8741fa6df72c731388` | 1,122,691,521 | 963,186,667 | 1,122,680,697 | 1,122,680,697 | 10,824 | 0.000964% |
| OpenStreetMap | `7b19c4f4716e9943b43ed923306e955bb54563f6f7cd57efd28f374fb8e523cd` | 94,622,364 | 82,860,302 | 94,616,443 | 94,616,443 | 5,921 | 0.006258% |
| Wikidata | `b56d86e3cf351e2689ab4d8e6e009638c3c34043875c189bf8d8178764f8af6c` | 334,585,518 | 294,320,602 | 334,489,532 | 334,489,532 | 95,986 | 0.028688% |

The Wikidata constructor produced a fully pointer-validated 334,558,422-byte blob, saving 27,096 bytes. Its remaining gap to the cyclic norm is 68,890 bytes (0.020591%). No larger candidate replaces the baseline.

| Corpus | Roots | EHOG nodes | Proper-overlap nodes | Components | Cut | MGreedyMin | Component joins | Join saving | Complete candidate |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| Common Crawl | 47,953,063 | 47,954,386 | 1,322 | 154 | 9,568 | 3,429,845,090 | not run | not run | 3,429,845,090 |
| GTA V | 55,448,624 | 57,719,188 | 2,270,563 | 1,027 | 16,042 | 1,122,696,739 | 622 | 1,227 | 1,122,695,512 |
| OpenStreetMap | 3,858,803 | 4,142,870 | 284,066 | 404 | 6,743 | 94,623,186 | 118 | 203 | 94,622,983 |
| Wikidata | 10,875,762 | 11,817,039 | 941,276 | 5,012 | 70,600 | 334,560,132 | 442 | 1,710 | 334,558,422 |

EHOG node counts include one empty node. The prior OSM report of 384,066 proper-overlap nodes was a 100,000-node transcription error: 3,858,803 roots + 284,066 nonempty proper-overlap nodes + 1 empty node = 4,142,870 total nodes.

## Cost

| Corpus | Evaluator | Full pack and evaluator | Memory scope |
|---|---:|---:|---|
| Common Crawl | 10m37.822s | 11m00.154s | separate monitored run: 4,841,298,016-byte pre-call heap, 9,866,044,192-byte sampled peak |
| GTA V | 7m43.231s | 8m16.219s | constructor monitored run: 3,501,568,968-byte pre-call heap, 18,490,427,320-byte sampled peak; 14m14.863s full call |
| OpenStreetMap | 20.208s | 22.461s | constructor monitored run: 1,561,152,784-byte pre-call heap, 3,314,340,960-byte sampled peak |
| Wikidata | 1m45.858s | 1m51.713s | constructor monitored run: 2,093,078,232-byte pre-call heap, 5,546,168,848-byte sampled peak; 2m15.780s full call |

The monitored runs are separate processes and are not timing-comparable with the timing-only rows. They sample `runtime.MemStats.Alloc` every millisecond, not RSS and can miss transient peaks.

## Constructors and limitations

The retained constructor extracts exact Euler cycles from Algorithm 2's residual graph, cuts each component at its cheapest root boundary and greedily joins complete positive-overlap component endpoints while preventing cycles. It searches hundreds or thousands of components rather than millions of roots. Every complete candidate is checked with the existing planner and every corpus pointer is resolved back to its original input.

An earlier arbitrary identity-pairing experiment preserved the Wikidata cyclic norm but created additional assignment cycles. Its cut was 198,447 rather than 70,600 and arbitrary joins produced 334,650,830 bytes. Exact Euler extraction superseded that implementation; the result demonstrates that preserving only the assignment weight is insufficient for a good linearization.

The current component join is greedy, so it does not establish the best choice of cycle cuts and joins. The remaining opportunities are joint cut/join optimization, alternative Euler traversals and bounded exchanges at baseline overlap conflicts. The certified gaps cap every conforming improvement, so the expensive machinery remains research-only.

## Reproduction

Run the exact bound and pointer validation:

```sh
GOMAXPROCS=32 SPACK_RESEARCH_CORPUS_PATH=/path/to/corpus.spc go test -run '^TestCyclicCoverResearchCorpus$' -count=1 -v
```

Also construct and conditionally apply a smaller candidate:

```sh
GOMAXPROCS=32 SPACK_RESEARCH_CORPUS_PATH=/path/to/corpus.spc SPACK_RESEARCH_CONSTRUCT=1 go test -run '^TestCyclicCoverResearchCorpus$' -count=1 -v
```

For a separate sampled-heap run, add `SPACK_RESEARCH_MEMORY=1`. Also pass `-timeout=20m` for GTA V; its monitored constructor run exceeds Go's default ten-minute test timeout.
