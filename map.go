package spack

import (
	"cmp"
	"fmt"
	"io"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unsafe"
)

const (
	// MaxStringLen is the maximum allowed length of a string to be packed.
	// This limit is constrained because the Pointer.Length field is a uint8.
	MaxStringLen = math.MaxUint8

	bucketSortParallelThreshold = 1 << 13
	numBuckets                  = 65793
)

// Pointer represents the location and length of a packed string within a PackedBlob.
type Pointer struct {
	// Offset is the byte offset of the string within the PackedBlob.
	Offset uint32
	// Length is the length of the string in bytes.
	Length uint8
}

// PackedBlob holds a single concatenated slice of bytes containing
// all the packed strings. Strings are retrieved using a Pointer.
type PackedBlob struct {
	blob []byte
}

// Pack compresses all strings currently collected in the StringMap into a PackedBlob.
// It deduplicates identical strings and leverages prefix/suffix overlaps to minimize
// the total binary footprint. It returns the packed blob alongside a slice of Pointers
// mapping each original string's index to its position in the blob.
//
// If any collected string exceeds MaxStringLen, an error is returned.
func (s *StringMap) Pack() (*PackedBlob, []Pointer, error) {
	s.mx.RLock()
	defer s.mx.RUnlock()

	length := len(s.entries)
	if length == 0 {
		return &PackedBlob{}, nil, nil
	}

	var (
		index                = make(map[string]int32, length)
		uniqueRepresentative = make([]int32, 0, length/2)
		uniqueID             = make([]int32, length)
	)

	for i, str := range s.entries {
		if len(str) > MaxStringLen {
			return nil, nil, fmt.Errorf("spack: entry %d length %d exceeds max %d", i, len(str), MaxStringLen)
		}

		id, ok := index[str]
		if !ok {
			id = int32(len(uniqueRepresentative))

			index[str] = id

			uniqueRepresentative = append(uniqueRepresentative, int32(i))
		}

		uniqueID[i] = id
	}

	numUnique := int32(len(uniqueRepresentative))

	index = nil // free

	uniqStrings := make([]string, numUnique)

	for i, idx := range uniqueRepresentative {
		uniqStrings[i] = s.entries[idx]
	}

	getPrefixBucket := func(s string) int {
		ln := len(s)
		if ln == 0 {
			return 0
		}

		if ln == 1 {
			return 1 + int(s[0])
		}

		return 257 + int(s[0])<<8 + int(s[1])
	}

	getSuffixBucket := func(s string) int {
		ln := len(s)
		if ln == 0 {
			return 0
		}

		if ln == 1 {
			return 1 + int(s[0])
		}

		return 257 + int(s[ln-1])<<8 + int(s[ln-2])
	}

	prefSorted := make([]int32, numUnique)

	for i := range prefSorted {
		prefSorted[i] = int32(i)
	}

	getPrefBucketUnique := func(uid int32) int {
		return getPrefixBucket(uniqStrings[uid])
	}

	compareNormalUnique := func(uid1, uid2 int32) int {
		return cmp.Compare(uniqStrings[uid1], uniqStrings[uid2])
	}

	suffSorted := make([]int32, numUnique)

	for i := range suffSorted {
		suffSorted[i] = int32(i)
	}

	getSuffBucketUnique := func(uid int32) int {
		return getSuffixBucket(uniqStrings[uid])
	}

	compareReversedUnique := func(uid1, uid2 int32) int {
		return compareReversed(uniqStrings[uid1], uniqStrings[uid2])
	}

	var wg sync.WaitGroup

	wg.Go(func() {
		bucketSort(prefSorted, getPrefBucketUnique, compareNormalUnique)
	})

	wg.Go(func() {
		bucketSort(suffSorted, getSuffBucketUnique, compareReversedUnique)
	})

	wg.Wait()

	parent := make([]int32, numUnique)

	for i := range parent {
		parent[i] = -1
	}

	parentOffset := make([]uint8, numUnique)

	for i := 0; i < len(prefSorted)-1; i++ {
		idxA := prefSorted[i]
		idxB := prefSorted[i+1]

		strA := uniqStrings[idxA]
		strB := uniqStrings[idxB]

		if strings.HasPrefix(strB, strA) {
			parent[idxA] = idxB
			parentOffset[idxA] = 0
		}
	}

	for i := 0; i < len(suffSorted)-1; i++ {
		idxA := suffSorted[i]
		idxB := suffSorted[i+1]

		if parent[idxA] == -1 {
			strA := uniqStrings[idxA]
			strB := uniqStrings[idxB]

			if strings.HasSuffix(strB, strA) {
				parent[idxA] = idxB
				parentOffset[idxA] = uint8(len(strB) - len(strA))
			}
		}
	}

	var blobCap int

	for i := range numUnique {
		if parent[i] == -1 {
			blobCap += len(uniqStrings[i])
		}
	}

	blob := make([]byte, 0, blobCap)

	resolvedOffset := make([]uint32, numUnique)

	for i := range resolvedOffset {
		resolvedOffset[i] = 0xFFFFFFFF
	}

	equalStrBytes := func(b []byte, str string) bool {
		if len(b) != len(str) {
			return false
		}

		if len(b) == 0 {
			return true
		}

		return unsafe.String(&b[0], len(b)) == str
	}

	for _, idx := range prefSorted {
		if parent[idx] == -1 {
			str := uniqStrings[idx]

			var (
				overlap    int
				maxOverlap = len(str)
			)

			if len(blob) < maxOverlap {
				maxOverlap = len(blob)
			}

			if maxOverlap > 255 {
				maxOverlap = 255
			}

			for k := maxOverlap; k > 0; k-- {
				if blob[len(blob)-k] == str[0] && blob[len(blob)-1] == str[k-1] {
					if equalStrBytes(blob[len(blob)-k:], str[:k]) {
						overlap = k

						break
					}
				}
			}

			if overlap > 0 {
				resolvedOffset[idx] = uint32(len(blob) - overlap)

				blob = append(blob, str[overlap:]...)
			} else {
				resolvedOffset[idx] = uint32(len(blob))

				blob = append(blob, str...)
			}
		}
	}

	var resolve func(idx int32) uint32

	resolve = func(idx int32) uint32 {
		if resolvedOffset[idx] != 0xFFFFFFFF {
			return resolvedOffset[idx]
		}

		pr := parent[idx]
		off := uint32(parentOffset[idx])

		prOff := resolve(pr)

		resolvedOffset[idx] = prOff + off

		return resolvedOffset[idx]
	}

	for i := range numUnique {
		resolve(i)
	}

	pointers := make([]Pointer, length)

	for i := range s.entries {
		uid := uniqueID[i]

		pointers[i] = Pointer{
			Offset: resolvedOffset[uid],
			Length: uint8(len(s.entries[i])),
		}
	}

	return &PackedBlob{blob: blob}, pointers, nil
}

// Get retrieves a string from the PackedBlob using the provided Pointer.
// If the Pointer's offset and length exceed the bounds of the blob,
// it returns an empty string and io.ErrUnexpectedEOF.
func (s *PackedBlob) Get(p Pointer) (string, error) {
	if int(p.Offset)+int(p.Length) > len(s.blob) {
		return "", io.ErrUnexpectedEOF
	}

	if p.Length == 0 {
		return "", nil
	}

	return unsafe.String(&s.blob[p.Offset], p.Length), nil
}

// Size returns the total size of the packed bytes in the blob.
func (s *PackedBlob) Size() int {
	return len(s.blob)
}

// Bytes returns the raw underlying byte slice of the PackedBlob.
// This slice should not be modified.
func (s *PackedBlob) Bytes() []byte {
	return s.blob
}

func bucketSort(indices []int32, getBucket func(int32) int, compare func(int32, int32) int) {
	length := len(indices)
	if length < 2 {
		return
	}

	if length < bucketSortParallelThreshold {
		slices.SortFunc(indices, compare)

		return
	}

	counts := make([]int32, numBuckets)

	for _, idx := range indices {
		counts[getBucket(idx)]++
	}

	offsets := make([]int32, numBuckets)

	var sum int32

	for i := range counts {
		offsets[i] = sum
		sum += counts[i]
	}

	temp := make([]int32, length)
	pos := make([]int32, numBuckets)

	copy(pos, offsets)

	for _, idx := range indices {
		bucket := getBucket(idx)

		temp[pos[bucket]] = idx

		pos[bucket]++
	}

	copy(indices, temp)

	bucketIdxChan := make(chan int, 2048)

	go func() {
		for i := range counts {
			if counts[i] >= 2 {
				bucketIdxChan <- i
			}
		}

		close(bucketIdxChan)
	}()

	var wg sync.WaitGroup

	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for bIdx := range bucketIdxChan {
				start, end := offsets[bIdx], offsets[bIdx]+counts[bIdx]

				slices.SortFunc(indices[start:end], compare)
			}
		})
	}

	wg.Wait()
}

func compareReversed(s1, s2 string) int {
	n1 := len(s1)
	n2 := len(s2)

	minLen := min(n2, n1)

	if minLen == 0 {
		if n1 < n2 {
			return -1
		}

		if n1 > n2 {
			return 1
		}

		return 0
	}

	// BCE
	_ = s1[n1-minLen]
	_ = s2[n2-minLen]

	for i := 1; i <= minLen; i++ {
		c1 := s1[n1-i]
		c2 := s2[n2-i]

		if c1 != c2 {
			if c1 < c2 {
				return -1
			}

			return 1
		}
	}

	if n1 < n2 {
		return -1
	}

	if n1 > n2 {
		return 1
	}

	return 0
}
