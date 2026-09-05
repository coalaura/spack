package spack_test

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/coalaura/spack"
)

type packTestCase struct {
	name        string
	input       []string
	wantBlobLen int // -1: only require blob <= payload
}

func TestPackEdgeCases(t *testing.T) {
	t.Parallel()

	allBytes := make([]string, 256)

	for i := range allBytes {
		allBytes[i] = string([]byte{byte(i)})
	}

	tests := []packTestCase{
		{"empty only", []string{""}, 0},
		{"duplicates", []string{"dup", "dup", "", "dup", ""}, 3},
		{"prefix and suffix containment chain", []string{"a", "ab", "abc", "abc", "b", "bc"}, 3},
		{"one-byte strings are contained", []string{"a", "ab", "b", "bcd", "cd", "d"}, 4},
		{"nul bytes", []string{"a", "a\x00", "\x00", "", "\x00a"}, 3},
		{"nul second byte", []string{"x\x00y", "x", "x\x00"}, 3},
		{"two-byte overlap", []string{"abcd", "cdef"}, 6},
		{"long overlaps", []string{"abcdefgh", "efghijkl", "ijklmnop"}, 16},
		{"seven byte overlap", []string{"abcdefghi", "cdefghixyz"}, 12},
		{"eight byte overlap", []string{"0abcdefgh", "abcdefgh1"}, 10},
		{"longest overlap wins", []string{"qqabcd", "abcdzz", "cdyy"}, 12},
		{"self overlap no cycle", []string{"abab"}, 4},
		{"two-node cycle refused", []string{"abab", "baba"}, 5},
		{"all single bytes", allBytes, 256},
		{"max length", []string{strings.Repeat("z", 255), strings.Repeat("z", 254)}, 255},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pack := packRoundTrip(t, tc.input)

			var payload int

			for _, str := range tc.input {
				payload += len(str)
			}

			if pack.Len() > payload {
				t.Fatalf("blob %d bytes exceeds payload %d", pack.Len(), payload)
			}

			if tc.wantBlobLen >= 0 && pack.Len() != tc.wantBlobLen {
				t.Fatalf("blob %d bytes, want %d (blob %q)", pack.Len(), tc.wantBlobLen, pack.Bytes())
			}
		})
	}
}

func TestPackRandomBinary(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(7, 11))

	alphabet := []byte{0, 1, 'a', 0xff}

	input := make([]string, 0, 20000)

	for range 20000 {
		buf := make([]byte, rng.IntN(13))

		for i := range buf {
			buf[i] = alphabet[rng.IntN(len(alphabet))]
		}

		input = append(input, string(buf))
	}

	packRoundTrip(t, input)
}

func TestPackRejectsTooLong(t *testing.T) {
	t.Parallel()

	_, err := spack.NewStringMap([]string{"ok", strings.Repeat("x", 256)}).Pack[spack.Pointer32]()
	if !errors.Is(err, spack.ErrStringTooLong) {
		t.Fatalf("got %v, want ErrStringTooLong", err)
	}
}

func TestPackDisableGC(t *testing.T) {
	t.Parallel()

	input := []string{"hello world", "world", "hello", "world"}

	options := spack.PackOptions{DisableGC: true}

	packRoundTrip(t, input, options)
}

func TestPackPointerWidths(t *testing.T) {
	t.Parallel()

	input := []string{"hello world", "world", "", "hello world"}

	testPackPointerWidth[spack.Pointer16](t, input, 3)
	testPackPointerWidth[spack.Pointer32](t, input, 5)
	testPackPointerWidth[spack.Pointer64](t, input, 9)
}

func TestPackPointer16Overflow(t *testing.T) {
	t.Parallel()

	input := make([]string, 300)

	for i := range input {
		input[i] = fmt.Sprintf("^%06d%s$", i, strings.Repeat("a", 247))
	}

	_, err := spack.NewStringMap(input).Pack[spack.Pointer16]()
	if !errors.Is(err, spack.ErrBlobTooLarge) {
		t.Fatalf("got %v, want ErrBlobTooLarge", err)
	}

	pack, err := spack.NewStringMap(input).Pack[spack.Pointer32]()
	if err != nil {
		t.Fatalf("Pack[Pointer32]: %v", err)
	}

	if pack.Len() != len(input)*spack.MaxStringLen {
		t.Fatalf("blob length %d, want %d", pack.Len(), len(input)*spack.MaxStringLen)
	}
}

func packRoundTrip(t *testing.T, input []string, options ...spack.PackOptions) *spack.PackedBlob[spack.Pointer32] {
	t.Helper()

	pack, err := spack.NewStringMap(input).Pack[spack.Pointer32](options...)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	pointers := pack.Pointers()
	if len(pointers) != len(input) {
		t.Fatalf("got %d pointers, want %d", len(pointers), len(input))
	}

	for i, want := range input {
		got, err := pack.GetStringUnsafe(pointers[i])
		if err != nil {
			t.Fatalf("index %d: %v", i, err)
		}

		if got != want {
			t.Fatalf("index %d: got %q, want %q", i, got, want)
		}
	}

	return pack
}

func testPackPointerWidth[T spack.PointerType](t *testing.T, input []string, pointerSize int) {
	t.Helper()

	pack, err := spack.NewStringMap(input).Pack[T]()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	wantSize := pack.Len() + pointerSize*len(input)
	if pack.Size() != wantSize {
		t.Fatalf("Size %d, want %d", pack.Size(), wantSize)
	}

	for i, pointer := range pack.Pointers() {
		got, err := spack.GetStringUnsafe(pack.Bytes(), pointer)
		if err != nil || got != input[i] {
			t.Fatalf("index %d: got %q, %v; want %q, nil", i, got, err, input[i])
		}
	}
}
