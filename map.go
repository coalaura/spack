package spack

import (
	"cmp"
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

// PackedBlob holds a single concatenated slice of bytes containing
// all the packed strings. Strings are retrieved using a Pointer.
type PackedBlob struct {
	pointers []Pointer
	blob     []byte
}

// Pack compresses all strings currently collected in the StringMap into a PackedBlob.
// It deduplicates identical strings and leverages prefix/suffix overlaps to minimize
// the total binary footprint. It returns the packed blob alongside a slice of Pointers
// mapping each original string's index to its position in the blob.
//
// If any collected string exceeds MaxStringLen, an error is returned.
func (s *StringMap) Pack() (*PackedBlob, error) {
	s.mx.RLock()
	defer s.mx.RUnlock()

	length := len(s.entries)
	if length == 0 {
		return &PackedBlob{}, nil
	}

	// sort indices to deduplicate without map overhead
	ids := make([]int32, length)

	for i := range ids {
		ids[i] = int32(i)
	}

	getPrefixBucket := func(str string) int {
		ln := len(str)
		if ln == 0 {
			return 0
		}

		if ln == 1 {
			return 1 + int(str[0])
		}

		_ = str[1] // BCE

		return 257 + int(str[0])<<8 + int(str[1])
	}

	getPrefBucket := func(idx int32) int {
		return getPrefixBucket(s.entries[idx])
	}

	compareNormal := func(idx1, idx2 int32) int {
		return cmp.Compare(s.entries[idx1], s.entries[idx2])
	}

	bucketSort(ids, getPrefBucket, compareNormal)

	// populate uniques from sorted array
	uniqueRepresentative := make([]int32, 0, length/2)
	uniqueID := make([]int32, length)

	var (
		lastStr    string
		currentUID int32 = -1
	)

	for _, origIdx := range ids {
		str := s.entries[origIdx]
		if currentUID == -1 || str != lastStr {
			currentUID++

			uniqueRepresentative = append(uniqueRepresentative, origIdx)

			lastStr = str
		}

		uniqueID[origIdx] = currentUID
	}

	numUnique := int32(len(uniqueRepresentative))

	ids = nil // free

	getUniqString := func(uid int32) string {
		return s.entries[uniqueRepresentative[uid]]
	}

	getSuffixBucket := func(str string) int {
		ln := len(str)
		if ln == 0 {
			return 0
		}

		if ln == 1 {
			return 1 + int(str[0])
		}

		_ = str[ln-1] // BCE
		_ = str[ln-2]

		return 257 + int(str[ln-1])<<8 + int(str[ln-2])
	}

	suffSorted := make([]int32, numUnique)

	for i := range suffSorted {
		suffSorted[i] = int32(i)
	}

	getSuffBucketUnique := func(uid int32) int {
		return getSuffixBucket(getUniqString(uid))
	}

	compareReversedUnique := func(uid1, uid2 int32) int {
		return compareReversed(getUniqString(uid1), getUniqString(uid2))
	}

	// deduplication already sorted uniqueRepresentative alphabetically, only sort suffixes
	bucketSort(suffSorted, getSuffBucketUnique, compareReversedUnique)

	parent := make([]int32, numUnique)

	for i := range parent {
		parent[i] = -1
	}

	parentOffset := make([]uint8, numUnique)

	numCPU := runtime.GOMAXPROCS(0)
	chunkSize := (int(numUnique) + numCPU - 1) / numCPU

	var wg sync.WaitGroup

	// parallel prefix overlapping loops
	for g := range numCPU {
		start := g * chunkSize
		end := min(start+chunkSize, int(numUnique)-1)

		if start >= end {
			continue
		}

		wg.Go(func() {
			for i := start; i < end; i++ {
				idxA := int32(i)
				idxB := int32(i + 1)

				strA := getUniqString(idxA)
				strB := getUniqString(idxB)

				if strings.HasPrefix(strB, strA) {
					parent[idxA] = idxB
				}
			}
		})
	}

	wg.Wait()

	// parallel suffix overlapping loops
	for g := range numCPU {
		start := g * chunkSize
		end := min(start+chunkSize, int(numUnique)-1)

		if start >= end {
			continue
		}

		wg.Go(func() {
			for i := start; i < end; i++ {
				idxA := suffSorted[i]
				idxB := suffSorted[i+1]

				if parent[idxA] == -1 {
					strA := getUniqString(idxA)
					strB := getUniqString(idxB)

					if strings.HasSuffix(strB, strA) {
						parent[idxA] = idxB
						parentOffset[idxA] = uint8(len(strB) - len(strA))
					}
				}
			}
		})
	}

	wg.Wait()

	var blobCap int

	for i := range numUnique {
		if parent[i] == -1 {
			blobCap += len(getUniqString(i))
		}
	}

	blob := make([]byte, 0, blobCap)

	resolvedOffset := make([]uint32, numUnique)

	for i := range resolvedOffset {
		resolvedOffset[i] = 0xFFFFFFFF
	}

	// overlap merging loop
	for idx := range numUnique {
		if parent[idx] == -1 {
			str := getUniqString(int32(idx))

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

			if maxOverlap > 0 {
				tail := blob[len(blob)-maxOverlap:]

				for k := maxOverlap; k > 0; k-- {
					if tail[maxOverlap-k] == str[0] && tail[maxOverlap-1] == str[k-1] {
						sub := tail[maxOverlap-k:]
						if unsafe.String(&sub[0], k) == str[:k] {
							overlap = k

							break
						}
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

	for i := range numUnique {
		idx := int32(i)
		curr := idx

		var (
			path  [256]int32
			depth int
		)

		for curr != -1 && resolvedOffset[curr] == 0xFFFFFFFF {
			path[depth] = curr
			depth++
			curr = parent[curr]
		}

		var baseOffset uint32

		if curr != -1 {
			baseOffset = resolvedOffset[curr]
		}

		for j := depth - 1; j >= 0; j-- {
			node := path[j]
			baseOffset += uint32(parentOffset[node])

			resolvedOffset[node] = baseOffset
		}
	}

	pointers := make([]Pointer, length)

	for i := range s.entries {
		uid := uniqueID[i]

		pointers[i] = NewPointer(resolvedOffset[uid], uint8(len(s.entries[i])))
	}

	return &PackedBlob{
		pointers: pointers,
		blob:     blob,
	}, nil
}

// GetStringUnsafe returns a zero-copy string pointing directly into the blob's memory.
// It is fast but unsafe: the returned string's lifetime is tied to the blob,
// and it will reflect any future modifications made to the underlying slice.
func (s *PackedBlob) GetStringUnsafe(pointer Pointer) (string, error) {
	return GetStringUnsafe(s.blob, pointer)
}

// GetString returns a copied, independent string from the PackedBlob.
// It allocates a new underlying buffer to ensure the returned string can
// safely outlive the blob and remains isolated from any future mutations.
func (s *PackedBlob) GetString(pointer Pointer) (string, error) {
	return GetString(s.blob, pointer)
}

// Pointers returns all pointers.
func (s *PackedBlob) Pointers() []Pointer {
	return s.pointers
}

// PackSize returns the total size of the packed bytes in the blob.
func (s *PackedBlob) PackSize() int {
	return len(s.blob)
}

// MemSize returns the total size of the packed bytes and pointers in memory.
func (s *PackedBlob) MemSize() int {
	return len(s.blob) + len(s.pointers)*int(unsafe.Sizeof(Pointer{}))
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
