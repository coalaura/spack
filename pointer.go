package spack

import (
	"io"
	"unsafe"
)

// Pointer represents the location and length of a packed string within a PackedBlob.
// To maximize memory compression, it is packed as a 5-byte array to avoid struct alignment padding.
type Pointer struct {
	buf [5]byte
}

// NewPointer initializes a new 5-byte packed Pointer.
func NewPointer(offset uint32, length uint8) Pointer {
	return Pointer{
		buf: [5]byte{
			byte(offset),
			byte(offset >> 8),
			byte(offset >> 16),
			byte(offset >> 24),
			length,
		},
	}
}

// Offset returns the byte offset of the string within the PackedBlob.
func (p Pointer) Offset() uint32 {
	return uint32(p.buf[0]) | uint32(p.buf[1])<<8 | uint32(p.buf[2])<<16 | uint32(p.buf[3])<<24
}

// Length returns the length of the string in bytes.
func (p Pointer) Length() uint8 {
	return p.buf[4]
}

// Bytes returns the internal 5-byte representation of the Pointer.
func (p Pointer) Bytes() [5]byte {
	return p.buf
}

// PointerFromBytes reconstructs a Pointer from a 5-byte array.
func PointerFromBytes(b [5]byte) Pointer {
	return Pointer{buf: b}
}

// PointerFromSlice reconstructs a Pointer from a slice.
// It returns an error if the slice contains fewer than 5 bytes.
func PointerFromSlice(b []byte) (Pointer, error) {
	if len(b) < 5 {
		return Pointer{}, io.ErrUnexpectedEOF
	}

	var arr [5]byte

	copy(arr[:], b[:5])

	return Pointer{buf: arr}, nil
}

// GetUnsafe returns a zero-copy string pointing directly into the blob's memory.
// It is fast but unsafe: the returned string's lifetime is tied to the blob,
// and it will reflect any future modifications made to the underlying slice.
func GetStringUnsafe(packed []byte, pointer Pointer) (string, error) {
	offset := pointer.Offset()
	length := pointer.Length()

	if int(offset)+int(length) > len(packed) {
		return "", io.ErrUnexpectedEOF
	}

	if length == 0 {
		return "", nil
	}

	return unsafe.String(&packed[offset], length), nil
}

// Get returns a copied, independent string from the packed blob.
// It allocates a new underlying buffer to ensure the returned string can
// safely outlive the blob and remains isolated from any future mutations.
func GetString(packed []byte, pointer Pointer) (string, error) {
	offset := pointer.Offset()
	length := pointer.Length()

	if int(offset)+int(length) > len(packed) {
		return "", io.ErrUnexpectedEOF
	}

	if length == 0 {
		return "", nil
	}

	buf := make([]byte, length)

	copy(buf, packed[offset:offset+uint32(length)])

	return unsafe.String(&buf[0], length), nil
}
