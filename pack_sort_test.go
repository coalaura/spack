package spack

import (
	"cmp"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type uniqueWordMarkerTest struct {
	name    string
	entries []string
}

func TestPackedBucketSort(t *testing.T) {
	t.Parallel()

	kinds := []string{
		"spread",
		"bucketed",
		"short",
		"duplicates",
		"shared prefix",
		"ordered",
		"reversed",
	}

	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			entries := sortCorpus(kind, 16385)
			directions := []bool{false, true}

			for _, reversed := range directions {
				name := "forward"

				if reversed {
					name = "reversed"
				}

				t.Run(name, func(t *testing.T) {
					original, offsets := partitionSortWords(entries, reversed)

					compare := func(a, b uint64) int {
						if reversed {
							if a>>32 != b>>32 {
								return cmp.Compare(a>>32, b>>32)
							}

							return compareReversed(
								entries[uint32(a)], entries[uint32(b)],
							)
						}

						return compareSortWord(a, b, entries)
					}

					// Sort the strings themselves, independently of the
					// cached-key layout and partition implementation.
					want := slices.Clone(entries)

					if reversed {
						slices.SortFunc(want, compareReversed)
					} else {
						slices.Sort(want)
					}

					workerCounts := []int{1, 4}

					for _, workers := range workerCounts {
						words := slices.Clone(original)

						sortPackedBuckets(words, offsets, workers, compare)

						seen := make([]bool, len(entries))

						for i, word := range words {
							input := uint32(word)

							if int(input) >= len(entries) || seen[input] {
								t.Fatalf("workers=%d: invalid/repeated index %d",
									workers, input)
							}

							seen[input] = true

							got := entries[input]

							if got != want[i] {
								t.Fatalf("workers=%d position=%d: got %q, want %q",
									workers, i, got, want[i])
							}
						}
					}
				})
			}
		})
	}
}

func TestUniqueWordMarkers(t *testing.T) {
	t.Parallel()

	tests := []uniqueWordMarkerTest{
		{"empty", nil},
		{"one empty", []string{""}},
		{
			"bucket boundaries and NUL padding",
			[]string{
				"", "", "\x00", "\x00\x00", "\x01", "\x01",
				"aa0123", "ab0123", "aa0123", "ab0123",
				"aa0123\x00", "aa0123\x00\x00",
				"\xff\x000123", "\xff\xff0123",
			},
		},
		{"duplicates across chunks", sortCorpus("duplicates", 8193)},
		{"mostly unique", sortCorpus("bucketed", 8193)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			original, offsets := partitionSortWords(tc.entries, false)

			sortPackedBuckets(original, offsets, 4, func(a, b uint64) int {
				return compareSortWord(a, b, tc.entries)
			})

			workerCounts := []int{1, 3, 32}

			for _, workers := range workerCounts {
				words := slices.Clone(original)

				chunkSize, chunkBase := markUniqueWords(words, tc.entries, workers)

				var count int32

				for i, word := range words {
					if i%chunkSize == 0 {
						got := chunkBase[i/chunkSize]

						if got != count {
							t.Fatalf("workers=%d position=%d: base %d, want %d",
								workers, i, got, count)
						}
					}

					input := uint32(original[i])
					unique := i == 0 ||
						tc.entries[input] != tc.entries[uint32(original[i-1])]

					got := word&sortWordUnique != 0

					if got != unique {
						t.Fatalf("workers=%d position=%d: marker %v, want %v",
							workers, i, got, unique)
					}

					index := uint32(word) & sortWordIndexMask

					if index != input {
						t.Fatalf("workers=%d position=%d: index %d, want %d",
							workers, i, index, input)
					}

					if word>>32 != original[i]>>32 {
						t.Fatalf("position=%d: marker changed cached key", i)
					}

					if unique {
						count++
					}
				}

				got := chunkBase[len(chunkBase)-1]

				if got != count {
					t.Fatalf("workers=%d: count %d, want %d",
						workers, got, count)
				}

				// Verify the same uid reconstruction used by Pack,
				// including chunks beginning inside a duplicate run.
				parallelChunks(len(words), chunkSize, func(chunk, start, end int) {
					uid := chunkBase[chunk] - 1

					for i := start; i < end; i++ {
						if words[i]&sortWordUnique != 0 {
							uid++
						}

						if uid < 0 || uid >= count {
							t.Errorf("workers=%d position=%d: invalid uid %d",
								workers, i, uid)
						}
					}
				})
			}
		})
	}
}

func BenchmarkPackedBucketSort(b *testing.B) {
	kinds := []string{
		"spread",
		"bucketed",
		"short",
		"duplicates",
		"shared prefix",
		"ordered",
		"reversed",
	}

	for _, kind := range kinds {
		b.Run(kind, func(b *testing.B) {
			entries := sortCorpus(kind, 1<<18)
			original, offsets := partitionSortWords(entries, false)
			numCPU := runtime.GOMAXPROCS(0)

			compare := func(a, b uint64) int {
				return compareSortWord(a, b, entries)
			}

			runRadix := []bool{false, true}

			for _, radix := range runRadix {
				name := "comparison"

				if radix {
					name = "radix"
				}

				b.Run(name, func(b *testing.B) {
					words := make([]uint64, len(original))

					b.ReportAllocs()

					for b.Loop() {
						// Both implementations include the same reset.
						// Re-sorting the previous result would otherwise
						// measure a different input distribution.
						copy(words, original)

						if radix {
							sortPackedBuckets(words, offsets, numCPU, compare)
						} else {
							sortBuckets(words, offsets, numCPU, compare)
						}
					}
				})
			}
		})
	}
}

func sortCorpus(kind string, count int) []string {
	rng := rand.New(rand.NewPCG(23, 59))
	entries := make([]string, count)

	for i := range entries {
		buf := make([]byte, 24)

		for j := range buf {
			buf[j] = byte(rng.Uint32())
		}

		switch kind {
		case "spread":
			entries[i] = string(buf)
		case "bucketed", "ordered", "reversed":
			buf[0] = 0
			buf[1] = 1
			entries[i] = string(buf)
		case "short":
			buf[0] = 0
			buf[1] = 1
			entries[i] = string(buf[:rng.IntN(8)])
		case "duplicates":
			entries[i] = fmt.Sprintf("value-%02d", rng.IntN(64))
		case "shared prefix":
			entries[i] = strings.Repeat("p", 80) + string(buf)
		default:
			panic("unknown sort corpus")
		}
	}

	switch kind {
	case "ordered":
		slices.Sort(entries)
	case "reversed":
		slices.Sort(entries)
		slices.Reverse(entries)
	}

	return entries
}

func partitionSortWords(entries []string, reversed bool) ([]uint64, []int32) {
	key := func(input int) uint64 {
		if reversed {
			return bits.ReverseBytes64(getSuffixWindow64(entries[input]))
		}

		return getPrefix64(entries[input])
	}

	var words []uint64

	offsets := bucketPartition(
		len(entries), 4,
		func(input int) int {
			return int(key(input) >> 48)
		},
		func(total int) {
			words = make([]uint64, total)
		},
		func(input int, pos int32) {
			words[pos] = key(input)<<16&highMask(4) | uint64(uint32(input))
		},
	)

	return words, offsets
}
