package spack

import (
	"io"
	"unsafe"
)

// PointerType is the set of packed pointer representations supported by Pack.
// The constraint intentionally specifies only concrete representations; their
// methods are not part of the constraint.
type PointerType interface {
	Pointer16 | Pointer32 | Pointer64
}

// Pointer16 stores a 16-bit offset and an 8-bit length without alignment padding.
type Pointer16 struct {
	buf [3]byte
}

// Pointer32 stores a 32-bit offset and an 8-bit length without alignment padding.
type Pointer32 struct {
	buf [5]byte
}

// Pointer64 stores a 64-bit offset and an 8-bit length without alignment padding.
type Pointer64 struct {
	buf [9]byte
}

// Offset returns the byte offset of the string within the packed blob.
func (p Pointer16) Offset() uint16 {
	return uint16(p.buf[0]) | uint16(p.buf[1])<<8
}

// Length returns the length of the string in bytes.
func (p Pointer16) Length() uint8 {
	return p.buf[2]
}

// Bytes returns the internal 3-byte representation of the pointer.
func (p Pointer16) Bytes() [3]byte {
	return p.buf
}

// Write writes the pointer to the writer.
func (p Pointer16) Write(wr io.Writer) (int, error) {
	return wr.Write(p.buf[:])
}

// Offset returns the byte offset of the string within the packed blob.
func (p Pointer32) Offset() uint32 {
	return uint32(p.buf[0]) | uint32(p.buf[1])<<8 | uint32(p.buf[2])<<16 | uint32(p.buf[3])<<24
}

// Length returns the length of the string in bytes.
func (p Pointer32) Length() uint8 {
	return p.buf[4]
}

// Bytes returns the internal 5-byte representation of the pointer.
func (p Pointer32) Bytes() [5]byte {
	return p.buf
}

// Write writes the pointer to the writer.
func (p Pointer32) Write(wr io.Writer) (int, error) {
	return wr.Write(p.buf[:])
}

// Offset returns the byte offset of the string within the packed blob.
func (p Pointer64) Offset() uint64 {
	return uint64(p.buf[0]) | uint64(p.buf[1])<<8 | uint64(p.buf[2])<<16 | uint64(p.buf[3])<<24 |
		uint64(p.buf[4])<<32 | uint64(p.buf[5])<<40 | uint64(p.buf[6])<<48 | uint64(p.buf[7])<<56
}

// Length returns the length of the string in bytes.
func (p Pointer64) Length() uint8 {
	return p.buf[8]
}

// Bytes returns the internal 9-byte representation of the pointer.
func (p Pointer64) Bytes() [9]byte {
	return p.buf
}

// Write writes the pointer to the writer.
func (p Pointer64) Write(wr io.Writer) (int, error) {
	return wr.Write(p.buf[:])
}

// NewPointer16 initializes a new 3-byte packed pointer.
func NewPointer16(offset uint16, length uint8) Pointer16 {
	return Pointer16{
		buf: [3]byte{
			byte(offset),
			byte(offset >> 8),
			length,
		},
	}
}

// NewPointer32 initializes a new 5-byte packed pointer.
func NewPointer32(offset uint32, length uint8) Pointer32 {
	return Pointer32{
		buf: [5]byte{
			byte(offset),
			byte(offset >> 8),
			byte(offset >> 16),
			byte(offset >> 24),
			length,
		},
	}
}

// NewPointer64 initializes a new 9-byte packed pointer.
func NewPointer64(offset uint64, length uint8) Pointer64 {
	return Pointer64{
		buf: [9]byte{
			byte(offset),
			byte(offset >> 8),
			byte(offset >> 16),
			byte(offset >> 24),
			byte(offset >> 32),
			byte(offset >> 40),
			byte(offset >> 48),
			byte(offset >> 56),
			length,
		},
	}
}

// ReadPointer16 reads a Pointer16 from a reader.
func ReadPointer16(rd io.Reader) (Pointer16, error) {
	var buf [3]byte

	_, err := io.ReadFull(rd, buf[:])
	if err != nil {
		return Pointer16{}, err
	}

	return Pointer16{buf: buf}, nil
}

// ReadPointer32 reads a Pointer32 from a reader.
func ReadPointer32(rd io.Reader) (Pointer32, error) {
	var buf [5]byte

	_, err := io.ReadFull(rd, buf[:])
	if err != nil {
		return Pointer32{}, err
	}

	return Pointer32{buf: buf}, nil
}

// ReadPointer64 reads a Pointer64 from a reader.
func ReadPointer64(rd io.Reader) (Pointer64, error) {
	var buf [9]byte

	_, err := io.ReadFull(rd, buf[:])
	if err != nil {
		return Pointer64{}, err
	}

	return Pointer64{buf: buf}, nil
}

// Pointer16FromBytes reconstructs a Pointer16 from a 3-byte array.
func Pointer16FromBytes(buf [3]byte) Pointer16 {
	return Pointer16{buf: buf}
}

// Pointer32FromBytes reconstructs a Pointer32 from a 5-byte array.
func Pointer32FromBytes(buf [5]byte) Pointer32 {
	return Pointer32{buf: buf}
}

// Pointer64FromBytes reconstructs a Pointer64 from a 9-byte array.
func Pointer64FromBytes(buf [9]byte) Pointer64 {
	return Pointer64{buf: buf}
}

// Pointer16FromSlice reconstructs a Pointer16 from the first three bytes of a slice.
func Pointer16FromSlice(buf []byte) (Pointer16, error) {
	if len(buf) < 3 {
		return Pointer16{}, io.ErrUnexpectedEOF
	}

	var value [3]byte

	copy(value[:], buf[:3])

	return Pointer16{buf: value}, nil
}

// Pointer32FromSlice reconstructs a Pointer32 from the first five bytes of a slice.
func Pointer32FromSlice(buf []byte) (Pointer32, error) {
	if len(buf) < 5 {
		return Pointer32{}, io.ErrUnexpectedEOF
	}

	var value [5]byte

	copy(value[:], buf[:5])

	return Pointer32{buf: value}, nil
}

// Pointer64FromSlice reconstructs a Pointer64 from the first nine bytes of a slice.
func Pointer64FromSlice(buf []byte) (Pointer64, error) {
	if len(buf) < 9 {
		return Pointer64{}, io.ErrUnexpectedEOF
	}

	var value [9]byte

	copy(value[:], buf[:9])

	return Pointer64{buf: value}, nil
}

// GetStringUnsafe returns a zero-copy string pointing directly into the blob's memory.
// It is fast but unsafe: the returned string's lifetime is tied to the blob,
// and it will reflect any future modifications made to the underlying slice.
func GetStringUnsafe[T PointerType](packed []byte, pointer T) (string, error) {
	offset, length := pointerOffsetAndLength(pointer)

	if offset > uint64(len(packed)) || uint64(length) > uint64(len(packed))-offset {
		return "", io.ErrUnexpectedEOF
	}

	if length == 0 {
		return "", nil
	}

	return unsafe.String(&packed[int(offset)], int(length)), nil
}

// GetString returns a copied, independent string from the packed blob.
// It allocates a new underlying buffer to ensure the returned string can
// safely outlive the blob and remains isolated from future mutations.
func GetString[T PointerType](packed []byte, pointer T) (string, error) {
	offset, length := pointerOffsetAndLength(pointer)

	if offset > uint64(len(packed)) || uint64(length) > uint64(len(packed))-offset {
		return "", io.ErrUnexpectedEOF
	}

	if length == 0 {
		return "", nil
	}

	start := int(offset)
	end := start + int(length)
	buf := make([]byte, int(length))

	copy(buf, packed[start:end])

	return unsafe.String(&buf[0], len(buf)), nil
}

func pointerOffsetAndLength[T PointerType](pointer T) (uint64, uint8) {
	switch value := any(pointer).(type) {
	case Pointer16:
		return uint64(value.Offset()), value.Length()
	case Pointer32:
		return uint64(value.Offset()), value.Length()
	case Pointer64:
		return value.Offset(), value.Length()
	default:
		panic("unsupported pointer type")
	}
}
