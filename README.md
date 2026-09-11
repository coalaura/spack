<picture>
  <source media="(prefers-color-scheme: dark)" srcset=".github/banner.svg">
  <source media="(prefers-color-scheme: light)" srcset=".github/banner-light.svg">
  <img alt="spack - Many strings. One contiguous blob." src=".github/banner-light.svg">
</picture>

spack is a minimal, high-performance string pack library for Go. It packs a collection of strings into a single, contiguous byte slice by deduplicating equal values and exploiting prefix, suffix, internal substring and suffix-to-prefix overlap relationships.

The resulting blob is flat, coherent and highly optimized for writing to a file and memory mapping (mmap).

## Key Features

* Single Coherent Blob: Packing emits a single byte slice and an array of compact 3-, 5- or 9-byte pointers. Perfect for direct disk serialization and zero-copy mmap.
* O(1) Lookups: Retrieving strings is a flat, simple offset lookup.
* Standalone Usability: GetString and GetStringUnsafe are decoupled from any struct. They operate directly on raw byte slices using Pointer16, Pointer32 or Pointer64, facilitating easy integration with mmap libraries.
* Zero Allocation Options: Unsafe retrieval returns views over the original block using Go string headers to avoid allocation.

## Performance

`spack` is benchmarked against several large, real-world corpora with substantially different string distributions:

* **Common Crawl URLs:** 50 million URL occurrences from the CC-MAIN-2026-34 columnar URL index. Duplicate occurrences are preserved and URLs are not normalized.
* **joaat.sh GTA V extraction:** 81.8 million unique strings extracted from 41.3 GB of GTA V/FiveM-oriented source code, scripts, build data and hash databases. This represents the original workload for which `spack` was developed.
* **OpenStreetMap tag values:** 50 million tag-value occurrences from Geofabrik extracts of Germany, Japan, Brazil and South Africa, balanced at 12.5 million values per region. Tag keys are excluded and duplicate values are preserved.
* **Wikidata labels and aliases:** 50 million multilingual label and alias occurrences from the 2026-08-31 entity dump. Descriptions, entity IDs and language codes are excluded; duplicate occurrences are preserved.

Values longer than 255 bytes are rejected rather than truncated.

| Corpus | Inputs | Logical input | Blob | Packed total | Blob-only saving | Total saving | Pack time |
|---|---:|---:|---:|---:|---:|---:|---:|
| Common Crawl URLs | 50.0M | 4.333 GB | 3.430 GB | 3.680 GB | 20.85% | 15.08% | 23.639 s |
| joaat.sh GTA V extraction | 81.8M | 3.048 GB | 1.123 GB | 1.532 GB | 63.16% | 49.74% | 33.849 s |
| OpenStreetMap tag values | 50.0M | 1.328 GB | 94.62 MB | 344.62 MB | 92.88% | 74.06% | 2.340 s |
| Wikidata labels and aliases | 50.0M | 1.778 GB | 334.59 MB | 584.59 MB | 81.18% | 67.12% | 5.799 s |

Sizes use decimal units (`1 MB = 1,000,000 bytes`, `1 GB = 1,000,000,000 bytes`). Packing times measure `Pack` only, with corpus loading and memory monitoring excluded.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset=".github/chart.svg">
  <source media="(prefers-color-scheme: light)" srcset=".github/chart-light.svg">
  <img alt="Packed representation across the benchmark corpora" src=".github/chart-light.svg">
</picture>

On this 64-bit system, the logical unpacked size is calculated as:

`string payload bytes + 16 × input count + 24`

The packed total below uses `Pointer32`:

`blob bytes + 5 × input count`

Accordingly, **blob-only saving** compares the blob against the logical unpacked representation and includes the removal of Go string headers. It is not a raw-payload compression ratio. **Total saving** includes the 5-byte `Pointer32` required for every input string.

### Certified blob-size bound

`Pack` can optionally calculate a proven lower bound on the size of any single raw-byte blob satisfying the same direct-view contract. This bounds blob bytes only, not pointer metadata or total process memory:

```go
var bound spack.BlobSizeBound

packed, err := sm.PackWithBlobSizeBound[spack.Pointer32](&bound)
```

The diagnostic runs on the final distinct, containment-reduced roots. This is sufficient for the original input because every removed value occurs inside a retained root, while every representation of the original input must also contain every retained root.

For each root, the diagnostic finds the complete maximum suffix-to-prefix overlap to any other root, separately for outgoing and incoming edges. An ordering of `n` substring-free roots uses at most `n-1` adjacent overlaps, leaving one outgoing and one incoming endpoint unused. Consequently, its overlap is at most both the sum of outgoing maxima minus their minimum and the corresponding incoming quantity. If `rootBytes` is the sum of root lengths, the certified bound is:

```text
overlapUpperBound = min(outgoingUpperBound, incomingUpperBound)
blobLowerBound = max(longestRoot, rootBytes - overlapUpperBound)
```

Substring-freeness ensures that root occurrences in a shortest blob can be ordered with strictly increasing starts and ends; removing gaps leaves exactly an adjacent-overlap construction. This establishes the ordering premise. Self-overlaps are excluded, arbitrary bytes are compared without normalization, and aggregate arithmetic is checked in `uint64`. Empty and single-root sets are handled directly.

The current blob is a feasible upper bound. Equality with the lower bound certifies optimal blob length; otherwise the true optimum lies in the reported interval. The gap is only the **maximum possible remaining saving**. A loose lower bound does not show that the gap is achievable.

Measurements from 2026-09-11 used Go 1.27.1 on Windows/amd64, an AMD Ryzen 9 9950X3D (32 logical CPUs), 61.68 GiB RAM, and repository revision `d95bea7b7143eaa5c1780a6ffa8088ff74fe7901` plus this diagnostic. Times are single runs. Normal `Pack` timing used no memory monitor; bound timing is the diagnostic's internal elapsed time during a separate monitored run.

| Corpus | Roots | Root bytes | Current blob | Lower bound | Maximum possible remaining saving | Gap | Pack time | Bound time |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Common Crawl URLs | 47,953,063 | 3,430,187,365 | 3,429,845,090 | 3,429,840,415 | 4,675 | 0.000136% | 23.639 s | 115.513 s |
| joaat.sh GTA V extraction | 55,448,624 | 1,196,266,294 | 1,122,691,521 | 963,186,667 | 159,504,854 | 14.207362% | 33.849 s | 138.108 s |
| OpenStreetMap tag values | 3,858,803 | 102,353,276 | 94,622,364 | 82,860,302 | 11,762,062 | 12.430531% | 2.340 s | 5.419 s |
| Wikidata labels and aliases | 10,875,762 | 361,584,132 | 334,585,518 | 294,320,602 | 40,264,916 | 12.034267% | 5.799 s | 20.369 s |

The overlap histograms below group each root's complete maximum by byte length. They help explain why the Common Crawl bound is tight in one direction: 99.50% of roots have no outgoing overlap, so the outgoing sum leaves little room above the constructed overlap.

| Corpus and direction | 0 bytes | 1-4 bytes | 5-8 bytes | 9+ bytes |
|---|---:|---:|---:|---:|
| Common Crawl outgoing | 47,711,831 | 239,705 | 150 | 1,377 |
| Common Crawl incoming | 0 | 0 | 26,938,673 | 21,014,390 |
| GTA V outgoing | 3,832,887 | 35,654,661 | 12,212,366 | 3,748,710 |
| GTA V incoming | 0 | 25,283,344 | 25,711,683 | 4,453,597 |
| OpenStreetMap outgoing | 6,404 | 2,426,526 | 872,532 | 553,341 |
| OpenStreetMap incoming | 5,976 | 1,964,930 | 1,128,901 | 758,996 |
| Wikidata outgoing | 530,319 | 4,308,987 | 3,841,020 | 2,195,436 |
| Wikidata incoming | 38,260 | 3,094,645 | 4,447,377 | 3,295,480 |

Peak heap was sampled from Go's `MemStats.Alloc` every 1 ms after a forced GC, and is reported as peak minus the already-loaded corpus. This scope covers the entire `Pack` invocation, not just bound-owned allocations, is not process RSS, and varies with GC timing. Matched single baseline/diagnostic-enabled runs observed 4,156.10/4,156.13 MiB for Common Crawl, 4,785.64/4,700.08 MiB for GTA V, 742.10/822.54 MiB for OpenStreetMap, and 1,162.57/1,380.61 MiB for Wikidata. The diagnostic raised the observed peak by 80.44 MiB for OpenStreetMap and 218.04 MiB for Wikidata; it did not raise the observed peak in the other two single-run pairs. The latter results are consistent with sampling and GC variance and do not imply that the diagnostic has no allocation cost.

The measured `.spc` artifacts are identified below. SHA-256 covers the complete file, including its `SPACKC01` header.

| Corpus file | SHA-256 |
|---|---|
| `commoncrawl-cc-main-2026-34-url-occurrences-50m.spc` | `8211781c1be4872600d5de07c4b53d49280ef5d7781123556a8cc71d5503df04` |
| `joaat-sh-gta-v-extracted-unique-81m.spc` | `fb282b81a78be350988314cc32bab1431d5eb052b1f7fc8741fa6df72c731388` |
| `osm-geofabrik-2026-09-05-tag-value-occurrences-balanced-50m.spc` | `7b19c4f4716e9943b43ed923306e955bb54563f6f7cd57efd28f374fb8e523cd` |
| `wikidata-20260831-label-alias-occurrences-50m.spc` | `b56d86e3cf351e2689ab4d8e6e009638c3c34043875c189bf8d8178764f8af6c` |

Reproduce a bound and sampled-heap run with:

```sh
SPACK_TEST_CORPUS_PATH="corpus/osm-geofabrik-2026-09-05-tag-value-occurrences-balanced-50m.spc" SPACK_TEST_BOUND=1 SPACK_TEST_MEMORY=1 go test -run '^TestPacker$' -count=1 -v
```

Run the same command without `SPACK_TEST_BOUND` and `SPACK_TEST_MEMORY` for an unmonitored packing baseline.

These results rule out further large-corpus compression work on Common Crawl: no conforming blob can save more than 4,675 additional bytes. The 12-14% gaps on the other corpora are unresolved, but do not predict recoverable savings. Prior experiments supplied with this benchmark context, and not rerun by this harness, found no OpenStreetMap gain from adjacent swaps; bounded relocation saved 39 bytes on OpenStreetMap and 155 bytes on Wikidata; conservative periodic relocation saved 34 bytes on OpenStreetMap, 209 bytes on GTA V, and nothing on Common Crawl. Their costs exceeded those savings, so repeating those same broad scans is not justified.

A stronger length-only bound is justified before another construction experiment. Cazaux, Juhel and Rivals, [“Practical lower and upper bounds for the Shortest Linear Superstring” (SEA 2018)](https://drops.dagstuhl.de/entities/document/10.4230/LIPIcs.SEA.2018.18), prove a stronger cyclic-cover lower bound and a constructible upper bound using `LCGreedyMin`/`MGreedyMin`. Their algorithm requires the paper's extended hierarchical overlap graph (EHOG), including non-maximal and self overlaps; neither the current greedy index nor a generic pairwise overlap graph is interchangeable with it. A faithful full-corpus implementation has material memory risk (the paper reports 46.6 million EHOG nodes using under 5.5 GB), especially for GTA V's 55.4 million roots. The worthwhile next experiment is therefore a bounded, length-only evaluator on OpenStreetMap first, then Wikidata, and GTA V only if memory scaling is acceptable. Construction integration and sparse competing-edge searches should wait for that evaluator to demonstrate a materially tighter interval.

## Usage

```go
package main

import (
	"fmt"
	"github.com/coalaura/spack"
)

func main() {
	// collect strings
	sm := spack.NewStringMap(nil)

	idx1, _ := sm.Add("hello world")
	idx2, _ := sm.Add("world")

	// pack strings into a flat blob
	packed, err := sm.Pack[spack.Pointer32]()
	if err != nil {
		panic(err)
	}

	rawBytes := packed.Bytes()
	pointers := packed.Pointers()

	// independent, O(1) lookups on the raw bytes
	str1, _ := spack.GetStringUnsafe(rawBytes, pointers[idx1])
	str2, _ := spack.GetStringUnsafe(rawBytes, pointers[idx2])

	fmt.Println(str1) // "hello world"
	fmt.Println(str2) // "world"
}
```

The pointer type is explicit: `Pointer16`, `Pointer32`, and `Pointer64` use 16-, 32-, and 64-bit blob offsets while retaining the same 8-bit string length. Choose the smallest type whose offset range can hold the packed blob.

`Pack` performs explicit garbage collections between memory-intensive phases by default. Disable those calls when latency is more important than reducing peak memory usage:

```go
packed, err := sm.Pack[spack.Pointer32](spack.PackOptions{DisableGC: true})
```

## Constraints

Individual strings cannot exceed 255 bytes (`MaxStringLen` is constrained by the 8-bit pointer length field). Packing returns `ErrBlobTooLarge` if any final string offset exceeds the selected pointer type's range.
