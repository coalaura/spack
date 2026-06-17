package spack

import (
	"fmt"
	"sync"
	"unsafe"
)

var ErrStringTooLong = fmt.Errorf("length exceeds max %d", MaxStringLen)

// StringMap is a thread-safe collector for building a list of strings
// to be packed together into a PackedBlob.
type StringMap struct {
	mx      sync.RWMutex
	length  uintptr
	entries []string
}

// NewStringMap initializes a new StringMap with an optional pre-filled entries slice.
func NewStringMap(entries []string) *StringMap {
	var length uintptr

	for _, str := range entries {
		length += uintptr(len(str))
	}

	return &StringMap{
		length:  length,
		entries: entries,
	}
}

// Add appends a string to the StringMap and returns its assigned index or ErrStringTooLong.
func (s *StringMap) Add(str string) (int, error) {
	if len(str) > MaxStringLen {
		return 0, ErrStringTooLong
	}

	s.mx.Lock()
	defer s.mx.Unlock()

	s.entries = append(s.entries, str)

	s.length += uintptr(len(str))

	return len(s.entries) - 1, nil
}

// AddUnsafe appends a string view of the provided byte slice to the StringMap
// and returns its assigned index or ErrStringTooLong. It avoids allocations by
// using unsafe-casting under the hood, but the caller must ensure the underlying
// byte slice is not mutated after this call.
func (s *StringMap) AddUnsafe(b []byte) (int, error) {
	if len(b) == 0 {
		return s.Add("")
	}

	if len(b) > MaxStringLen {
		return 0, ErrStringTooLong
	}

	str := unsafe.String(&b[0], len(b))

	s.mx.Lock()
	defer s.mx.Unlock()

	s.entries = append(s.entries, str)

	s.length += uintptr(len(str))

	return len(s.entries) - 1, nil
}

// GetString returns the string at the specified index.
// It will panic if the index is out of bounds.
func (s *StringMap) GetString(index int) string {
	s.mx.RLock()
	defer s.mx.RUnlock()

	return s.entries[index]
}

// Size returns the total in-memory size of the entries slice and its strings.
func (s *StringMap) Size() int {
	s.mx.RLock()
	defer s.mx.RUnlock()

	var dummy string

	size := unsafe.Sizeof(s.entries)
	size += uintptr(len(s.entries)) * unsafe.Sizeof(dummy)

	return int(s.length + size)
}

// Length returns the number of strings currently collected in the StringMap.
func (s *StringMap) Length() int {
	s.mx.RLock()
	defer s.mx.RUnlock()

	return len(s.entries)
}

// Strings returns the underlying slice of collected strings.
//
// WARNING: This returns a direct reference to the internal slice.
// Modifying the returned slice or accessing it concurrently while other
// goroutines call Add or AddUnsafe is not thread-safe and will cause data races.
func (s *StringMap) Strings() []string {
	s.mx.RLock()
	defer s.mx.RUnlock()

	return s.entries
}
