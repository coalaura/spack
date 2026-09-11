package spack_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"unsafe"

	"github.com/coalaura/spack"
)

type exactCapacityTestCase struct {
	name  string
	input []string
}

type pointerTestCase struct {
	name    string
	blob    []byte
	pointer spack.Pointer32
	want    string
	wantErr bool
}

type pointerGetter struct {
	name string
	get  func([]byte, spack.Pointer32) (string, error)
}

type pointerSizeTestCase struct {
	name string
	got  uintptr
	want uintptr
}

type packingQualityTestCase struct {
	name  string
	input []string
}

func TestPackInternalContainment(t *testing.T) {
	t.Parallel()

	lengths := []int{1, 2, 3, 7, 8, 9, 64, 253}

	for _, length := range lengths {
		t.Run(fmt.Sprintf("length %d", length), func(t *testing.T) {
			t.Parallel()

			child := strings.Repeat("\x00", length)
			host := "\x01" + child + "\xff"

			pack := packRoundTrip(t, []string{child, host, child, ""})

			if pack.Len() != len(host) {
				t.Fatalf("blob %d bytes, want %d", pack.Len(), len(host))
			}
		})
	}
}

func TestPackLongOverlapSelection(t *testing.T) {
	t.Parallel()

	tests := []packTestCase{
		{
			name: "sixteen bytes beats eight",
			input: []string{
				"!abcdefghijklmnop",
				"abcdefghijklmnop?",
				"ijklmnop#",
			},
			wantBlobLen: 27,
		},
		{
			name: "another head after first is consumed",
			input: []string{
				"0abcdefghij",
				"1abcdefghij",
				"abcdefghij2",
				"abcdefghij3",
			},
			wantBlobLen: 24,
		},
		{
			name:        "retry after own chain head",
			input:       []string{"abAab", "abB", "zab"},
			wantBlobLen: 7,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pack := packRoundTrip(t, tc.input)

			if pack.Len() != tc.wantBlobLen {
				t.Fatalf("blob %d bytes, want %d (blob %q)",
					pack.Len(), tc.wantBlobLen, pack.Bytes())
			}
		})
	}
}

func TestPackEqualOverlapSelection(t *testing.T) {
	t.Parallel()

	pack := packRoundTrip(t, []string{"abb", "bba", "bbb"})
	if pack.Len() != 5 {
		t.Fatalf("blob %d bytes, want 5 (blob %q)", pack.Len(), pack.Bytes())
	}

	if string(pack.Bytes()) != "abbba" {
		t.Fatalf("blob %q, want deterministic blob %q", pack.Bytes(), "abbba")
	}
}

func TestPackSmallInstancesMatchExactSolution(t *testing.T) {
	t.Parallel()

	tests := []packingQualityTestCase{
		{
			name:  "competing equal overlaps",
			input: []string{"abb", "bba", "bbb"},
		},
		{
			name:  "directed cycle",
			input: []string{"abcd", "cdab", "dabc"},
		},
		{
			name:  "periodic strings",
			input: []string{"aaba", "abaa", "baab"},
		},
		{
			name:  "arbitrary bytes",
			input: []string{"\x00\xffa", "a\x00\xff", "\xffa\x00"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pack := packRoundTrip(t, tc.input)
			want := exactSuperstringLength(tc.input)

			if pack.Len() != want {
				t.Fatalf("blob %d bytes, exact solution is %d (blob %q)",
					pack.Len(), want, pack.Bytes())
			}
		})
	}
}

func TestPackFullLengthOverlaps(t *testing.T) {
	t.Parallel()

	lengths := []int{9, 16, 64, 254}

	for _, length := range lengths {
		t.Run(fmt.Sprintf("overlap %d", length), func(t *testing.T) {
			t.Parallel()

			shared := strings.Repeat("\x80", length)
			pack := packRoundTrip(t, []string{
				"\x01" + shared,
				shared + "\xff",
			})

			if pack.Len() != length+2 {
				t.Fatalf("blob %d bytes, want %d", pack.Len(), length+2)
			}
		})
	}
}

func TestPackPaddedPrefixBoundaries(t *testing.T) {
	t.Parallel()

	// Include every short string over an alphabet containing both extremes,
	// plus ties crossing the four-, six-, and eight-byte cached-key limits.
	input := []string{""}
	level := []string{""}
	alphabet := []byte{0, 1, 0xff}

	for range 3 {
		next := make([]string, 0, len(level)*len(alphabet))

		for _, prefix := range level {
			for _, value := range alphabet {
				next = append(next, prefix+string([]byte{value}))
			}
		}

		input = append(input, next...)
		level = next
	}

	input = append(input,
		"a",
		"a\x00",
		"a\x00\x00\x00",
		"a\x00\x00\x00\x00",
		"a\x00\x00\x00\x00\x00",
		"a\x00\x00\x00\x00\x00\x00\x00",
		"a\x00\x00\x00\x00\x00\x00\x00\x00",
		"a\x00\x00\x00\x00\x00\x00\x00\x01",
		"\x01\x00\x00\x00\x00\x00\x00\x00a",
		"\x00\x00\x00\x00\x00\x00\x00\x00a",
	)

	original := append([]string(nil), input...)
	input = append(input, original...)

	packRoundTrip(t, input)
}

func TestPackExactCapacity(t *testing.T) {
	t.Parallel()

	tests := []exactCapacityTestCase{
		{"no inputs", nil},
		{"empty values", []string{"", "", ""}},
		{
			"incidental boundary overlaps",
			[]string{
				"0abcdefghijklmnop",
				"abcdefghijklmnop1",
				"p1XYZ",
				"XYZtail",
				"internal",
				"!internal?",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pack := packRoundTrip(t, tc.input)

			if cap(pack.Bytes()) != len(pack.Bytes()) {
				t.Fatalf("blob len %d, cap %d", len(pack.Bytes()), cap(pack.Bytes()))
			}

			if cap(pack.Pointers()) != len(pack.Pointers()) {
				t.Fatalf("pointers len %d, cap %d",
					len(pack.Pointers()), cap(pack.Pointers()))
			}

			wantSize := len(pack.Bytes()) + 5*len(tc.input)

			if pack.Size() != wantSize {
				t.Fatalf("Size %d, want %d", pack.Size(), wantSize)
			}
		})
	}
}

func TestPointerBoundsAndLayout(t *testing.T) {
	t.Parallel()

	sizes := []pointerSizeTestCase{
		{"Pointer16", unsafe.Sizeof(spack.Pointer16{}), 3},
		{"Pointer32", unsafe.Sizeof(spack.Pointer32{}), 5},
		{"Pointer64", unsafe.Sizeof(spack.Pointer64{}), 9},
	}

	for _, tc := range sizes {
		if tc.got != tc.want {
			t.Fatalf("%s occupies %d bytes, want %d", tc.name, tc.got, tc.want)
		}
	}

	pointer := spack.NewPointer32(0x12345678, 0xff)
	wantBytes := [5]byte{0x78, 0x56, 0x34, 0x12, 0xff}

	if pointer.Bytes() != wantBytes {
		t.Fatalf("serialized pointer %x, want %x", pointer.Bytes(), wantBytes)
	}

	tests := []pointerTestCase{
		{"empty", nil, spack.NewPointer32(0, 0), "", false},
		{"empty at end", []byte("a"), spack.NewPointer32(1, 0), "", false},
		{"empty beyond end", nil, spack.NewPointer32(1, 0), "", true},
		{"last byte", []byte{0, 0xff}, spack.NewPointer32(1, 1), "\xff", false},
		{"past end", []byte("a"), spack.NewPointer32(1, 1), "", true},
		{"max offset", nil, spack.NewPointer32(math.MaxUint32, 0), "", true},
		{"wrapping end", []byte("a"), spack.NewPointer32(math.MaxUint32, 255), "", true},
	}

	getters := []pointerGetter{
		{"unsafe", func(blob []byte, pointer spack.Pointer32) (string, error) {
			return spack.GetStringUnsafe(blob, pointer)
		}},
		{"copied", func(blob []byte, pointer spack.Pointer32) (string, error) {
			return spack.GetString(blob, pointer)
		}},
	}

	for _, getter := range getters {
		t.Run(getter.name, func(t *testing.T) {
			t.Parallel()

			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					got, err := getter.get(tc.blob, tc.pointer)

					if tc.wantErr {
						if !errors.Is(err, io.ErrUnexpectedEOF) {
							t.Fatalf("got %v, want io.ErrUnexpectedEOF", err)
						}

						return
					}

					if err != nil || got != tc.want {
						t.Fatalf("got %q, %v; want %q, nil", got, err, tc.want)
					}
				})
			}
		})
	}
}

func TestPointerSerialization(t *testing.T) {
	t.Parallel()

	pointer16 := spack.NewPointer16(0x1234, 0x56)
	want16 := [3]byte{0x34, 0x12, 0x56}

	if pointer16.Offset() != 0x1234 || pointer16.Length() != 0x56 || pointer16.Bytes() != want16 {
		t.Fatalf("Pointer16 encoded as %x", pointer16.Bytes())
	}

	pointer32 := spack.NewPointer32(0x12345678, 0x9a)
	want32 := [5]byte{0x78, 0x56, 0x34, 0x12, 0x9a}

	if pointer32.Offset() != 0x12345678 || pointer32.Length() != 0x9a || pointer32.Bytes() != want32 {
		t.Fatalf("Pointer32 encoded as %x", pointer32.Bytes())
	}

	pointer64 := spack.NewPointer64(0x123456789abcdef0, 0xbc)
	want64 := [9]byte{0xf0, 0xde, 0xbc, 0x9a, 0x78, 0x56, 0x34, 0x12, 0xbc}

	if pointer64.Offset() != 0x123456789abcdef0 || pointer64.Length() != 0xbc || pointer64.Bytes() != want64 {
		t.Fatalf("Pointer64 encoded as %x", pointer64.Bytes())
	}

	var written bytes.Buffer

	n, err := pointer64.Write(&written)
	if err != nil || n != len(want64) || !bytes.Equal(written.Bytes(), want64[:]) {
		t.Fatalf("Pointer64.Write wrote %x, %d, %v", written.Bytes(), n, err)
	}

	read64, err := spack.ReadPointer64(bytes.NewReader(want64[:]))
	if err != nil || read64 != pointer64 {
		t.Fatalf("ReadPointer64 returned %x, %v", read64.Bytes(), err)
	}

	if spack.Pointer16FromBytes(want16) != pointer16 {
		t.Fatal("Pointer16FromBytes did not reconstruct the pointer")
	}

	from32, err := spack.Pointer32FromSlice(append(want32[:], 0xff))
	if err != nil || from32 != pointer32 {
		t.Fatalf("Pointer32FromSlice returned %x, %v", from32.Bytes(), err)
	}

	_, err = spack.Pointer64FromSlice(want64[:len(want64)-1])
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Pointer64FromSlice returned %v, want io.ErrUnexpectedEOF", err)
	}

	_, err = spack.GetString(nil, spack.NewPointer64(math.MaxUint64, 1))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("GetString returned %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestGetStringUnsafeDoesNotAllocate(t *testing.T) {
	pack := packRoundTrip(t, []string{"a\x00b"})
	pointer := pack.Pointers()[0]

	allocs := testing.AllocsPerRun(1000, func() {
		got, err := pack.GetStringUnsafe(pointer)
		if err != nil || got != "a\x00b" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	if allocs != 0 {
		t.Fatalf("GetStringUnsafe allocated %g times, want 0", allocs)
	}
}

func TestPackCachedKeyAcrossBuckets(t *testing.T) {
	t.Parallel()

	packRoundTrip(t, []string{
		"",
		"\x00",
		"\x00\x00",
		"\x01",
		"\x01\x00",
		"aa0123",
		"ab0123",
		"ac0123",
		"aa0123",
		"aa0123\x00",
		"ab0123\x00",
		"\xff\x000123",
		"\xff\xff0123",
	})
}

func BenchmarkPackSmallAmbiguous(b *testing.B) {
	collector := spack.NewStringMap(nil)
	input := []string{"abb", "bba", "bbb"}

	for _, value := range input {
		_, err := collector.Add(value)
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.SetBytes(9)

	var (
		blobBytes   int
		packedBytes int
	)

	for b.Loop() {
		pack, err := collector.Pack[spack.Pointer32](spack.PackOptions{DisableGC: true})
		if err != nil {
			b.Fatal(err)
		}

		blobBytes = pack.Len()
		packedBytes = pack.Size()
	}

	b.ReportMetric(7, "baseline-blob-bytes")
	b.ReportMetric(float64(blobBytes), "blob-bytes")
	b.ReportMetric(float64(packedBytes), "packed-bytes")
}

func exactSuperstringLength(input []string) int {
	used := make([]bool, len(input))
	best := math.MaxInt

	searchSuperstringOrders(input, used, -1, 0, 0, &best)

	return best
}

func searchSuperstringOrders(input []string, used []bool, previous, depth, length int, best *int) {
	if depth == len(input) {
		*best = min(*best, length)

		return
	}

	for current, value := range input {
		if used[current] {
			continue
		}

		nextLength := len(value)

		if previous != -1 {
			nextLength += length - testStringOverlap(input[previous], value)
		}

		if nextLength >= *best {
			continue
		}

		used[current] = true

		searchSuperstringOrders(input, used, current, depth+1, nextLength, best)

		used[current] = false
	}
}

func testStringOverlap(previous, current string) int {
	for overlap := min(len(previous), len(current)); overlap > 0; overlap-- {
		if previous[len(previous)-overlap:] == current[:overlap] {
			return overlap
		}
	}

	return 0
}
