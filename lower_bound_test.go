package spack

import (
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

const denseSharedAffixRootCount = 96
const denseSharedAffixRootLength = 80

type blobBoundTestCase struct {
	name              string
	input             []string
	checkExpectations bool
	wantRoots         int
	wantRootBytes     uint64
	wantLowerBound    uint64
	wantOptimal       int
}

func TestIndexedMaximumOverlapsMatchExhaustiveSearch(t *testing.T) {
	t.Parallel()

	tests := []blobBoundTestCase{
		{
			name: "no roots",
		},
		{
			name:  "one empty root",
			input: []string{""},
		},
		{
			name:  "periodic self overlaps",
			input: []string{"abab", "baba", "cdcd", "dcdc"},
		},
		{
			name: "competing directions",
			input: []string{
				"aaab", "aaba", "abaa", "baaa", "bbba",
			},
		},
		{
			name: "short arbitrary bytes",
			input: []string{
				"\x00\xffa", "a\x00\xff", "\xffa\x00", "\x80\x00b",
			},
		},
		{
			name: "seven eight and nine byte overlap boundaries",
			input: []string{
				"A1234567", "1234567B",
				"C12345678", "12345678D",
				"E123456789", "123456789F",
			},
		},
		{
			name: "maximum length boundary",
			input: []string{
				"0" + strings.Repeat("a", MaxStringLen-1),
				strings.Repeat("a", MaxStringLen-1) + "1",
				"2" + strings.Repeat("b", MaxStringLen-1),
			},
		},
		{
			name:  "overlapping lengths from nine through 255 bytes",
			input: overlappingLengthRoots(),
		},
		{
			name:  "dense long shared prefixes and suffixes",
			input: denseSharedAffixRoots(),
		},
		{
			name: "periodic roots exclude self overlaps",
			input: []string{
				strings.Repeat("ab", 96),
				strings.Repeat("cd", 96),
				strings.Repeat("\x00\xff", 96),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			roots := oracleSubstringFreeRoots(tc.input)

			gotOutgoing, gotIncoming := indexedMaximumOverlaps(t, roots)
			wantOutgoing, wantIncoming := exhaustiveMaximumOverlaps(roots)

			if !slices.Equal(gotOutgoing, wantOutgoing) {
				t.Fatalf("outgoing maxima %v, want %v", gotOutgoing, wantOutgoing)
			}

			if !slices.Equal(gotIncoming, wantIncoming) {
				t.Fatalf("incoming maxima %v, want %v", gotIncoming, wantIncoming)
			}
		})
	}
}

func TestBlobSizeBoundAgainstExactOracle(t *testing.T) {
	t.Parallel()

	tests := []blobBoundTestCase{
		{
			name:              "empty input",
			checkExpectations: true,
			wantRoots:         0,
			wantRootBytes:     0,
			wantLowerBound:    0,
			wantOptimal:       0,
		},
		{
			name:              "empty strings",
			input:             []string{"", "", ""},
			checkExpectations: true,
			wantRoots:         1,
			wantRootBytes:     0,
			wantLowerBound:    0,
			wantOptimal:       0,
		},
		{
			name:              "duplicates and containment",
			input:             []string{"", "bc", "abcd", "bc", "abcd", "c"},
			checkExpectations: true,
			wantRoots:         1,
			wantRootBytes:     4,
			wantLowerBound:    4,
			wantOptimal:       4,
		},
		{
			name:              "no overlap",
			input:             []string{"ab", "cd", "ef"},
			checkExpectations: true,
			wantRoots:         3,
			wantRootBytes:     6,
			wantLowerBound:    6,
			wantOptimal:       6,
		},
		{
			name:              "tight chain",
			input:             []string{"abcd", "cdef", "efgh"},
			checkExpectations: true,
			wantRoots:         3,
			wantRootBytes:     12,
			wantLowerBound:    8,
			wantOptimal:       8,
		},
		{
			name:              "loose disconnected cycles",
			input:             []string{"abab", "baba", "cdcd", "dcdc"},
			checkExpectations: true,
			wantRoots:         4,
			wantRootBytes:     16,
			wantLowerBound:    7,
			wantOptimal:       10,
		},
		{
			name: "competing incoming and outgoing",
			input: []string{
				"abcx", "bcxy", "cxyz", "yabc",
			},
		},
		{
			name: "arbitrary bytes",
			input: []string{
				"\x00\xffa", "a\x00\xff", "\xffa\x00", "\x80\x00b",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assertBoundAgainstOracle(t, tc.input, tc)
		})
	}
}

func TestPackWithBlobSizeBoundMatchesPackAboveRefinementLimit(t *testing.T) {
	t.Parallel()

	input := overlappingRoots(12, 48)

	input = append(input, input[3], input[7][8:32], "")

	options := PackOptions{
		DisableGC: true,
	}

	ordinary, err := NewStringMap(input).Pack[Pointer32](options)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	var bound BlobSizeBound

	diagnostic, err := NewStringMap(input).PackWithBlobSizeBound[Pointer32](&bound, options)
	if err != nil {
		t.Fatalf("PackWithBlobSizeBound: %v", err)
	}

	if bound.RootCount <= smallRefinementRootLimit {
		t.Fatalf("root count %d does not exceed refinement limit %d", bound.RootCount, smallRefinementRootLimit)
	}

	if !slices.Equal(ordinary.Bytes(), diagnostic.Bytes()) {
		t.Fatal("diagnostic-enabled blob differs from ordinary blob")
	}

	if !slices.Equal(ordinary.Pointers(), diagnostic.Pointers()) {
		t.Fatal("diagnostic-enabled pointers differ from ordinary pointers")
	}

	for index, pointer := range diagnostic.Pointers() {
		value, getErr := diagnostic.GetStringUnsafe(pointer)
		if getErr != nil {
			t.Fatalf("GetStringUnsafe(%d): %v", index, getErr)
		}

		if value != input[index] {
			t.Fatalf("value %d = %q, want %q", index, value, input[index])
		}
	}
}

func TestBlobSizeBoundRandomizedAgainstExactOracle(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(0x8f41, 0x19b7))
	alphabet := []byte{0, 1, 'a', 'b', 0x80, 0xff}

	for iteration := range 300 {
		count := rng.IntN(8)
		input := make([]string, count)

		for index := range input {
			buffer := make([]byte, rng.IntN(8))

			for position := range buffer {
				buffer[position] = alphabet[rng.IntN(len(alphabet))]
			}

			input[index] = string(buffer)
		}

		roots := oracleSubstringFreeRoots(input)
		if len(roots) > 10 {
			t.Fatalf("iteration %d produced %d oracle roots", iteration, len(roots))
		}

		gotOutgoing, gotIncoming := indexedMaximumOverlaps(t, roots)
		wantOutgoing, wantIncoming := exhaustiveMaximumOverlaps(roots)

		if !slices.Equal(gotOutgoing, wantOutgoing) {
			t.Fatalf("iteration %d outgoing maxima %v, want %v", iteration, gotOutgoing, wantOutgoing)
		}

		if !slices.Equal(gotIncoming, wantIncoming) {
			t.Fatalf("iteration %d incoming maxima %v, want %v", iteration, gotIncoming, wantIncoming)
		}

		name := blobBoundTestCase{
			name: "random",
		}

		assertBoundAgainstOracle(t, input, name)
	}
}

func assertBoundAgainstOracle(t *testing.T, input []string, tc blobBoundTestCase) {
	t.Helper()

	bound := BlobSizeBound{
		RootCount:       -1,
		LowerBoundBytes: math.MaxUint64,
	}

	options := PackOptions{
		DisableGC: true,
	}

	pack, err := NewStringMap(input).PackWithBlobSizeBound[Pointer32](&bound, options)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	optimal := exactOracleSuperstringLength(input)
	if bound.LowerBoundBytes > uint64(optimal) {
		t.Fatalf("lower bound %d exceeds exact optimum %d", bound.LowerBoundBytes, optimal)
	}

	if optimal > pack.Len() {
		t.Fatalf("exact optimum %d exceeds current blob %d", optimal, pack.Len())
	}

	if bound.CurrentBlobBytes != uint64(pack.Len()) {
		t.Fatalf("reported blob %d, want %d", bound.CurrentBlobBytes, pack.Len())
	}

	if tc.checkExpectations && bound.RootCount != tc.wantRoots {
		t.Fatalf("roots %d, want %d", bound.RootCount, tc.wantRoots)
	}

	if tc.checkExpectations && bound.RootBytes != tc.wantRootBytes {
		t.Fatalf("root bytes %d, want %d", bound.RootBytes, tc.wantRootBytes)
	}

	if tc.checkExpectations && bound.LowerBoundBytes != tc.wantLowerBound {
		t.Fatalf("lower bound %d, want %d", bound.LowerBoundBytes, tc.wantLowerBound)
	}

	if tc.checkExpectations && optimal != tc.wantOptimal {
		t.Fatalf("exact optimum %d, want %d", optimal, tc.wantOptimal)
	}
}

func indexedMaximumOverlaps(t *testing.T, roots []string) ([]uint8, []uint8) {
	t.Helper()

	rootIDs := make([]int32, len(roots))
	prefix := make([]uint64, len(roots))
	length := make([]uint8, len(roots))

	for position, root := range roots {
		rootIDs[position] = int32(position)
		prefix[position] = getPrefix64(root)
		length[position] = uint8(len(root))
	}

	if len(roots) <= 1 {
		return make([]uint8, len(roots)), make([]uint8, len(roots))
	}

	index := newRootIndexWithShort(roots, rootIDs, rootIDs, prefix, length, false)
	outgoing := make([]uint8, len(roots))

	for tail := range roots {
		outgoing[tail] = uint8(longestCompleteOverlap(index, int32(tail)))
	}

	reverse := newReverseOverlapIndex(roots, rootIDs, rootIDs, length)
	incoming := make([]uint8, len(roots))

	for tail := range roots {
		position := reverse.positions[tail]
		incoming[position] = uint8(reverse.longestOverlap(int32(tail)))
	}

	return outgoing, incoming
}

func exhaustiveMaximumOverlaps(roots []string) ([]uint8, []uint8) {
	outgoing := make([]uint8, len(roots))
	incoming := make([]uint8, len(roots))

	for tail, tailString := range roots {
		for head, headString := range roots {
			if tail == head {
				continue
			}

			overlap := uint8(oracleStringOverlap(tailString, headString))

			outgoing[tail] = max(outgoing[tail], overlap)
			incoming[head] = max(incoming[head], overlap)
		}
	}

	return outgoing, incoming
}

func exactOracleSuperstringLength(input []string) int {
	roots := oracleSubstringFreeRoots(input)
	if len(roots) == 0 {
		return 0
	}

	stateCount := 1 << uint(len(roots))
	scores := make([]int, stateCount*len(roots))

	for index := range scores {
		scores[index] = math.MaxInt
	}

	for root, value := range roots {
		state := 1 << uint(root)
		scores[state*len(roots)+root] = len(value)
	}

	for state := 1; state < stateCount; state++ {
		for tail := range roots {
			length := scores[state*len(roots)+tail]
			if length == math.MaxInt {
				continue
			}

			for head, value := range roots {
				bit := 1 << uint(head)
				if state&bit != 0 {
					continue
				}

				nextState := state | bit
				nextLength := length + len(value) - oracleStringOverlap(roots[tail], value)
				index := nextState*len(roots) + head

				scores[index] = min(scores[index], nextLength)
			}
		}
	}

	best := math.MaxInt
	finalState := stateCount - 1

	for tail := range roots {
		best = min(best, scores[finalState*len(roots)+tail])
	}

	return best
}

func oracleSubstringFreeRoots(input []string) []string {
	unique := slices.Clone(input)

	slices.Sort(unique)

	unique = slices.Compact(unique)

	roots := make([]string, 0, len(unique))

	for candidate, value := range unique {
		contained := false

		for host, hostValue := range unique {
			if candidate != host && strings.Contains(hostValue, value) {
				contained = true

				break
			}
		}

		if !contained {
			roots = append(roots, value)
		}
	}

	return roots
}

func oracleStringOverlap(tail, head string) int {
	maximum := min(len(tail), len(head))

	for overlap := maximum; overlap > 0; overlap-- {
		if tail[len(tail)-overlap:] == head[:overlap] {
			return overlap
		}
	}

	return 0
}

func overlappingLengthRoots() []string {
	roots := make([]string, 0, MaxStringLen-8)
	rng := rand.New(rand.NewPCG(0x713b, 0xc4e9))
	affix := [4]byte{0x00, 0xff, 0x80, 0x01}

	for length := 9; length <= MaxStringLen; length++ {
		value := make([]byte, length)

		copy(value, affix[:])
		copy(value[length-len(affix):], affix[:])

		for position := len(affix); position < length-len(affix); position++ {
			value[position] = byte(rng.Uint32())
		}

		roots = append(roots, string(value))
	}

	return roots
}

func denseSharedAffixRoots() []string {
	roots := make([]string, 0, denseSharedAffixRootCount)
	affix := []byte("\x00\xffshared-radix-affix\x80")

	for index := range denseSharedAffixRootCount {
		value := make([]byte, denseSharedAffixRootLength)

		copy(value, affix)
		copy(value[denseSharedAffixRootLength-len(affix):], affix)

		for position := len(affix); position < denseSharedAffixRootLength-len(affix); position++ {
			value[position] = byte(index*37 + position*19)
		}

		value[len(affix)] = byte(index)
		roots = append(roots, string(value))
	}

	return roots
}

func overlappingRoots(count, length int) []string {
	roots := make([]string, count)
	tokens := make([][8]byte, count+1)

	for token := range tokens {
		for position := range tokens[token] {
			tokens[token][position] = byte(token*29 + position*17)
		}
	}

	for index := range roots {
		value := make([]byte, length)

		copy(value, tokens[index][:])
		copy(value[length-len(tokens[index+1]):], tokens[index+1][:])

		for position := len(tokens[index]); position < length-len(tokens[index+1]); position++ {
			value[position] = byte(index*41 + position*23)
		}

		roots[index] = string(value)
	}

	return roots
}
