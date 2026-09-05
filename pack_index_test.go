package spack

import (
	"cmp"
	"slices"
	"strings"
	"testing"
)

type containmentScanTestCase struct {
	name    string
	entries []string
	want    []uint8
}

// contained returns the root that is a prefix of suffix, if any.
// Prefix-freeness means there can be at most one.
func (r *rootIndex) contained(suffix string) int32 {
	child, _ := r.containedAndOverlap(suffix, -1, false)

	return child
}

func TestCompareSortWordMatchesLexicographicOrder(t *testing.T) {
	t.Parallel()

	entries := []string{
		"",
		"\x00",
		"\x00\x00",
		"\x00\x00\x00",
		"\x00\x01",
		"\x01",
		"a",
		"a\x00",
		"a\x00\x00\x00",
		"a\x00\x00\x00\x00",
		"a\x00\x00\x00\x00\x00",
		"a\x00\x00\x00\x00\x00\x00",
		"a\x00\x00\x00\x00\x01",
		"a\x00\x00\x00\x01",
		"abcd",
		"abcd\x00",
		"abcd\x00\x00",
		"abcd\x00\x00\x00",
		"abcdef",
		"abcdef\x00",
		"abcdef\x01",
		"abcdefghi",
		"abcdefxyz",
		"abcdeg",
		"\xff",
		"\xff\x00",
		"\xff\xff",
	}

	for i, first := range entries {
		for j, second := range entries {
			prefixA := getPrefix64(first)
			prefixB := getPrefix64(second)

			if prefixA>>48 != prefixB>>48 {
				continue
			}

			wordA := prefixA<<16&highMask(4) | uint64(uint32(i))
			wordB := prefixB<<16&highMask(4) | uint64(uint32(j))

			got := compareSortWord(wordA, wordB, entries)
			want := cmp.Compare(first, second)

			if cmp.Compare(got, 0) != cmp.Compare(want, 0) {
				t.Fatalf("compare %q, %q: got %d, want sign of %d",
					first, second, got, want)
			}
		}
	}
}

func TestRootIndexMatchesPrefixFreeDictionary(t *testing.T) {
	t.Parallel()

	entries := []string{
		"\x00",
		"\x01\x00",
		"\x01\x01\xff",
		"abc",
		"abd\x00\x00",
		"abcdefghX", // Removed below: "abc" is its prefix.
		"longlong",
		"longlonger", // Removed below: "longlong" is its prefix.
		"samekey!one",
		"samekey!two",
		"\xff\x00\x00\x00\x00\x00\x00\x00",
		"\xff\x00\x00\x00\x00\x00\x00\x01x",
	}

	slices.Sort(entries)

	// Keep the shortest representative of each prefix range to construct
	// a prefix-free dictionary independently of the production packer.
	dictionary := make([]string, 0, len(entries))

	for _, str := range entries {
		if len(dictionary) != 0 && strings.HasPrefix(str, dictionary[len(dictionary)-1]) {
			continue
		}

		dictionary = append(dictionary, str)
	}

	roots := make([]int32, len(dictionary))
	prefix := make([]uint64, len(dictionary))
	length := make([]uint8, len(dictionary))

	for i, str := range dictionary {
		roots[i] = int32(i)
		prefix[i] = getPrefix64(str)
		length[i] = uint8(len(str))
	}

	index := newRootIndex(dictionary, roots, roots, prefix, length)

	queries := []string{
		"",
		"\x02",
		"ab",
		"abce",
		"abd\x00",
		"longlon",
		"samekey!",
		"samekey!three",
		"\xff\x00\x00\x00\x00\x00\x00\x00tail",
	}

	for _, str := range dictionary {
		queries = append(queries, str, str+"\x00suffix", str+"\xff")

		for end := range len(str) {
			queries = append(queries, str[:end])
		}
	}

	for _, query := range queries {
		want := int32(-1)

		for i, str := range dictionary {
			if strings.HasPrefix(query, str) {
				want = int32(i)

				break
			}
		}

		got := index.contained(query)

		if got != want {
			t.Fatalf("contained(%q) = %d, want %d", query, got, want)
		}
	}
}

func TestContainmentScanFindsLongOverlapCandidates(t *testing.T) {
	t.Parallel()

	tests := []containmentScanTestCase{
		{
			name: "overlap shorter than every root",
			entries: []string{
				"0123456789abcdefghi",
				"abcdefghiJKLMNOPQRST",
			},
			want: []uint8{9, 0},
		},
		{
			name: "skip own head",
			entries: []string{
				"abcdefghi!abcdefghi",
				"abcdefghi~",
			},
			want: []uint8{9, 0},
		},
		{
			name: "contained root is not an overlap head",
			entries: []string{
				"!abcdefghi?",
				"abcdefghi",
			},
			want: []uint8{0, 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if !slices.IsSorted(tc.entries) {
				t.Fatal("test entries must be lexicographically sorted")
			}

			roots := make([]int32, len(tc.entries))
			prefix := make([]uint64, len(tc.entries))
			length := make([]uint8, len(tc.entries))
			parent := make([]int32, len(tc.entries))
			parentOffset := make([]uint8, len(tc.entries))

			for i, str := range tc.entries {
				roots[i] = int32(i)
				prefix[i] = getPrefix64(str)
				length[i] = uint8(len(str))
				parent[i] = -1
			}

			got := removeInternalContainment(
				tc.entries, roots, roots, prefix, length,
				parent, parentOffset, 2,
			)

			if !slices.Equal(got, tc.want) {
				t.Fatalf("candidates %v, want %v", got, tc.want)
			}

			if tc.name == "contained root is not an overlap head" &&
				(parent[1] != 0 || parentOffset[1] != 1) {
				t.Fatalf("contained root parent %d, offset %d; want 0, 1",
					parent[1], parentOffset[1])
			}
		})
	}
}

func TestAvailableRootPreservesPendingLinks(t *testing.T) {
	t.Parallel()

	workspace := make([]uint64, 4)

	for i := range workspace {
		workspace[i] = uint64(uint32(i))<<32 | uint64(uint32(100+i))
	}

	// Delete heads 1, 2 and 3, forming a path to the implicit sentinel.
	roots := []int32{1, 2, 3}

	for _, root := range roots {
		next := availableRoot(workspace, root+1)

		workspace[root] = uint64(uint32(next))<<32 |
			uint64(uint32(workspace[root]))
	}

	got := availableRoot(workspace, 0)

	if got != 0 {
		t.Fatalf("availableRoot(0) = %d, want 0", got)
	}

	got = availableRoot(workspace, 1)

	if got != 4 {
		t.Fatalf("availableRoot(1) = %d, want sentinel 4", got)
	}

	got = availableRoot(workspace, 4)

	if got != 4 {
		t.Fatalf("availableRoot(sentinel) = %d, want 4", got)
	}

	for i, word := range workspace {
		got := uint32(word)

		if got != uint32(100+i) {
			t.Fatalf("pending link %d = %d, want %d", i, got, 100+i)
		}
	}
}

func TestPlanBlobMatchesFullBlob(t *testing.T) {
	t.Parallel()

	entries := make([]string, 0, 4096)

	var state uint32 = 1

	for i := range 4096 {
		buf := make([]byte, 1+i%MaxStringLen)

		for j := range buf {
			state = state*1664525 + 1013904223
			buf[j] = byte(state >> 24)
		}

		entries = append(entries, string(buf))
	}

	entries = append(entries,
		"",
		strings.Repeat("\x00", MaxStringLen),
		strings.Repeat("\x00", MaxStringLen),
		strings.Repeat("\x00", MaxStringLen-1)+"\xff",
		"\xff",
		"",
	)

	roots := make([]int32, len(entries))
	resolvedOffset := make([]uint64, len(entries))
	chains := newRootChains(len(entries), make([]uint8, len(entries)))

	for i := range roots {
		roots[i] = int32(i)
	}

	blobLen, err := planBlob(entries, roots, roots, chains, resolvedOffset)
	if err != nil {
		t.Fatalf("planBlob: %v", err)
	}

	var blob []byte

	for i, str := range entries {
		// Independent reference: search against the actual accumulated
		// blob, not the planner's bounded history or findOverlap helper.
		overlap := min(len(blob), len(str))

		for overlap > 0 {
			if string(blob[len(blob)-overlap:]) == str[:overlap] {
				break
			}

			overlap--
		}

		wantOffset := uint64(len(blob) - overlap)

		if resolvedOffset[i] != wantOffset {
			t.Fatalf("root %d: offset %d, want %d",
				i, resolvedOffset[i], wantOffset)
		}

		if int(chains.overlap[i]) != overlap {
			t.Fatalf("root %d: overlap %d, want %d",
				i, chains.overlap[i], overlap)
		}

		blob = append(blob, str[overlap:]...)
	}

	if blobLen != len(blob) {
		t.Fatalf("planned length %d, want %d", blobLen, len(blob))
	}
}

func TestRootIndexRadixMatchesLinearSearch(t *testing.T) {
	t.Parallel()

	entries := make([]string, 4096)

	for i := range entries {
		// One large two-byte bucket, with populated and empty intervals
		// farther into the key. Every string has the same length, making
		// the dictionary prefix-free.
		entries[i] = string([]byte{
			0x12, 0x34, byte(i >> 4), byte(i << 4),
			'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h',
		})
	}

	roots := make([]int32, len(entries))
	prefix := make([]uint64, len(entries))
	length := make([]uint8, len(entries))

	for i, str := range entries {
		roots[i] = int32(i)
		prefix[i] = getPrefix64(str)
		length[i] = uint8(len(str))
	}

	index := newRootIndex(entries, roots, roots, prefix, length)

	if index.radixBits[0x1234] == 0 {
		t.Fatal("large bucket was not subdivided")
	}

	check := func(query string) {
		t.Helper()

		wantChild := int32(-1)
		wantHead := int32(-1)

		for root, str := range entries {
			if strings.HasPrefix(query, str) {
				wantChild = int32(root)
			}

			if wantHead == -1 && strings.HasPrefix(str, query) {
				wantHead = int32(root)
			}
		}

		child, overlap := index.containedAndOverlap(query, -1, true)

		wantOverlap := wantChild == -1 &&
			len(query) > maxOverlapLevel &&
			wantHead != -1

		if child != wantChild || overlap != wantOverlap {
			t.Fatalf("query %q: got child %d, overlap %v; want %d, %v",
				query, child, overlap, wantChild, wantOverlap)
		}

		if len(query) > maxOverlapLevel {
			got := index.overlapHead(query, -1, nil)

			if got != wantHead {
				t.Fatalf("overlapHead(%q) = %d, want %d",
					query, got, wantHead)
			}
		}
	}

	for i := 0; i < len(entries); i += 31 {
		str := entries[i]

		check(str)
		check(str + "\x00tail")
		check(str[:9])

		absent := []byte(str)
		absent[3]++

		check(string(absent))
	}

	check("\x12\x33abcdefghij")
	check("\x12\x35abcdefghij")
}

func TestShortIndexMatchesLinearSearch(t *testing.T) {
	t.Parallel()

	entries := []string{
		"\x00\x00",
		"\x00\x01\x00",
		"ab\xff",
		"ac",
		"adabcdefgh",
		"z",
	}

	for i := range 32 {
		for j := range 16 {
			buf := []byte{'a', 'b', byte(i), byte(j)}

			for range i % 4 {
				buf = append(buf, 0)
			}

			entries = append(entries, string(buf))
		}
	}

	slices.Sort(entries)

	// All entries must be prefix-free for the predecessor search.
	for i := 1; i < len(entries); i++ {
		if strings.HasPrefix(entries[i], entries[i-1]) {
			t.Fatalf("test dictionary is not prefix-free: %q, %q",
				entries[i-1], entries[i])
		}
	}

	roots := make([]int32, len(entries))
	prefix := make([]uint64, len(entries))
	length := make([]uint8, len(entries))

	for i, str := range entries {
		roots[i] = int32(i)
		prefix[i] = getPrefix64(str)
		length[i] = uint8(len(str))
	}

	index := newRootIndex(entries, roots, roots, prefix, length)

	if len(index.shortTwo) == 0 || len(index.shortThird) == 0 {
		t.Fatal("test did not construct both short indexes")
	}

	check := func(query string) {
		t.Helper()

		want := int32(-1)

		for root, str := range entries {
			if len(str) >= 2 && len(str) < 8 &&
				strings.HasPrefix(query, str) {
				want = int32(root)

				break
			}
		}

		got := index.shortContained(query, getPrefix64(query))
		if got != want {
			t.Fatalf("shortContained(%q) = %d, want %d", query, got, want)
		}
	}

	for _, str := range entries {
		check(str)
		check(str + "\x00tail")
		check(str + "\xff")

		for end := range len(str) {
			check(str[:end])
		}
	}

	queries := []string{
		"",
		"a",
		"ab",
		"ab\x80missing",
		"ab\xfe",
		"ab\x00\xffmissing",
		"acanything",
		"aeanything",
		"\x00\x00suffix",
		"\x00\x01",
		"\x00\x01\x01",
	}

	for _, query := range queries {
		check(query)
	}
}

func TestLongRootIndexOmitsShortStorage(t *testing.T) {
	t.Parallel()

	entries := []string{
		"aa",
		"bbb",
		"cccccccccc",
	}

	roots := make([]int32, len(entries))
	prefix := make([]uint64, len(entries))
	length := make([]uint8, len(entries))

	for i, str := range entries {
		roots[i] = int32(i)
		prefix[i] = getPrefix64(str)
		length[i] = uint8(len(str))
	}

	index := newRootIndexWithShort(
		entries, roots, roots, prefix, length, false,
	)

	if len(index.short) != 0 ||
		len(index.shortKeys) != 0 ||
		len(index.shortBuckets) != 0 ||
		len(index.shortTwo) != 0 ||
		len(index.shortThird) != 0 {
		t.Fatal("long-only index allocated short-string storage")
	}

	got := index.overlapHead("ccccccccc", -1, nil)

	if got != 2 {
		t.Fatalf("overlapHead = %d, want 2", got)
	}
}
