package spack_test

import (
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
	pointer spack.Pointer
	want    string
	wantErr bool
}

type pointerGetter struct {
	name string
	get  func([]byte, spack.Pointer) (string, error)
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

	got := unsafe.Sizeof(spack.Pointer{})

	if got != 5 {
		t.Fatalf("Pointer occupies %d bytes, want 5", got)
	}

	pointer := spack.NewPointer(0x12345678, 0xff)
	wantBytes := [5]byte{0x78, 0x56, 0x34, 0x12, 0xff}

	if pointer.Bytes() != wantBytes {
		t.Fatalf("serialized pointer %x, want %x", pointer.Bytes(), wantBytes)
	}

	tests := []pointerTestCase{
		{"empty", nil, spack.NewPointer(0, 0), "", false},
		{"empty at end", []byte("a"), spack.NewPointer(1, 0), "", false},
		{"empty beyond end", nil, spack.NewPointer(1, 0), "", true},
		{"last byte", []byte{0, 0xff}, spack.NewPointer(1, 1), "\xff", false},
		{"past end", []byte("a"), spack.NewPointer(1, 1), "", true},
		{"max offset", nil, spack.NewPointer(math.MaxUint32, 0), "", true},
		{"wrapping end", []byte("a"), spack.NewPointer(math.MaxUint32, 255), "", true},
	}

	getters := []pointerGetter{
		{"unsafe", spack.GetStringUnsafe},
		{"copied", spack.GetString},
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
