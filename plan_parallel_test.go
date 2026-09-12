package spack

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

type substringFreePlannerTestCase struct {
	name    string
	entries []string
}

func TestSubstringFreePlannerMatchesSerial(t *testing.T) {
	t.Parallel()

	tests := []substringFreePlannerTestCase{
		{
			name: "no roots",
		},
		{
			name:    "empty root",
			entries: []string{""},
		},
		{
			name:    "one root",
			entries: []string{"abab"},
		},
		{
			name: "long overlaps",
			entries: []string{
				"0abcdefghijklmnop",
				"abcdefghijklmnop1",
				"mnop1tail",
				"tailXYZ",
			},
		},
		{
			name: "binary strings",
			entries: []string{
				"\x00\x01\xff",
				"\x01\xff\x80",
				"\xff\x80\x00",
				"\x80\x00\x01",
			},
		},
		{
			name: "maximum overlap",
			entries: []string{
				"\x01" + strings.Repeat("\x80", 254),
				strings.Repeat("\x80", 254) + "\xff",
			},
		},
	}

	rng := rand.New(rand.NewPCG(19, 37))
	random := make([]string, 256)

	for i := range random {
		buf := make([]byte, 1+rng.IntN(64))

		for j := range buf {
			buf[j] = byte(rng.IntN(8))
		}

		random[i] = string(buf)
	}

	// Construct a substring-free dictionary independently of the packer.
	slices.Sort(random)
	random = slices.Compact(random)

	filtered := make([]string, 0, len(random))

	for i, str := range random {
		contained := false

		for j, host := range random {
			if i != j && strings.Contains(host, str) {
				contained = true

				break
			}
		}

		if !contained {
			filtered = append(filtered, str)
		}
	}

	tests = append(tests, substringFreePlannerTestCase{
		name:    "random substring-free roots",
		entries: filtered,
	})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := slices.Clone(tc.entries)
			slices.Sort(entries)

			for i, str := range entries {
				for j, host := range entries {
					if i != j && strings.Contains(host, str) {
						t.Fatalf("test roots are not substring-free: %q inside %q", str, host)
					}
				}
			}

			representatives := make([]int32, len(entries))
			roots := make([]int32, len(entries))
			length := make([]uint8, len(entries))

			for i, str := range entries {
				representatives[i] = int32(i)
				roots[i] = int32(i)
				length[i] = uint8(len(str))
			}

			linkedValues := []bool{false, true}

			for _, linked := range linkedValues {
				name := "separate chains"

				if linked {
					name = "mixed chains"
				}

				t.Run(name, func(t *testing.T) {
					chains := newRootChains(
						len(entries), make([]uint8, len(entries)),
					)

					if linked {
						rng := rand.New(rand.NewPCG(43, 71))
						order := rng.Perm(len(entries))

						for i := 1; i < len(order); i++ {
							// Leave several chain boundaries in the
							// emission sequence as well as linked edges.
							if i%5 == 0 {
								continue
							}

							tail := int32(order[i-1])
							head := int32(order[i])

							known := 0

							if i%2 == 0 {
								// Independent reference for a verified
								// lower bound. Other edges use zero.
								previous := entries[tail]
								str := entries[head]

								known = min(len(previous), len(str))

								for known > 0 &&
									!strings.HasSuffix(previous, str[:known]) {
									known--
								}
							}

							chains.link(tail, head, known)
						}
					}

					serial := &rootChains{
						succ:    slices.Clone(chains.succ),
						hasPred: slices.Clone(chains.hasPred),
						overlap: slices.Clone(chains.overlap),
					}

					parallel := &rootChains{
						succ:    slices.Clone(chains.succ),
						hasPred: slices.Clone(chains.hasPred),
						overlap: slices.Clone(chains.overlap),
					}

					wantOffsets := make([]uint64, len(entries))
					gotOffsets := make([]uint64, len(entries))

					wantLen, err := planBlob(entries, representatives, roots, serial, wantOffsets)
					if err != nil {
						t.Fatal(err)
					}

					gotLen, err := planSubstringFreeBlob(entries, representatives, roots, length, parallel, gotOffsets, 4)
					if err != nil {
						t.Fatal(err)
					}

					if gotLen != wantLen {
						t.Fatalf("length %d, want %d", gotLen, wantLen)
					}

					if !slices.Equal(gotOffsets, wantOffsets) {
						t.Fatalf("offsets %v, want %v", gotOffsets, wantOffsets)
					}

					if !slices.Equal(parallel.overlap, serial.overlap) {
						t.Fatalf("overlaps %v, want %v", parallel.overlap, serial.overlap)
					}
				})
			}
		})
	}
}
