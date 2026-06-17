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

	numPrefixBuckets = 65793
)

// PackedBlob holds a single concatenated slice of bytes containing
// all the packed strings. Strings are retrieved using a Pointer.
type PackedBlob struct {
	pointers []Pointer
	blob     []byte
}

type sortKey struct {
	prefix uint64
	idx    int32
}

type suffixKey struct {
	suffix uint64
	uid    int32
}

// Pack compresses all strings currently collected in the StringMap into a PackedBlob.
func (s *StringMap) Pack() (*PackedBlob, error) {
	s.mx.RLock()
	defer s.mx.RUnlock()

	length := len(s.entries)
	if length == 0 {
		return &PackedBlob{}, nil
	}

	numCPU := runtime.GOMAXPROCS(0)

	// build robust sortKeys with 8-byte prefix cache
	keys := make([]sortKey, length)
	chunkSize := (length + numCPU - 1) / numCPU

	var wg sync.WaitGroup

	for g := range numCPU {
		start := g * chunkSize
		end := min(start+chunkSize, length)

		if start >= end {
			continue
		}

		wg.Go(func() {
			for i := start; i < end; i++ {
				keys[i] = sortKey{
					idx:    int32(i),
					prefix: getPrefix64(s.entries[i]),
				}
			}
		})
	}

	wg.Wait()

	bucketSortKeys(keys, s.entries)

	// deduplicate using cached prefix to avoid s.entries random accesses
	uniqueRepresentative := make([]int32, 0, length/2)
	uniqueID := make([]int32, length)

	var (
		lastPrefix uint64
		lastStr    string
		currentUID int32 = -1
	)

	for _, k := range keys {
		origIdx := k.idx

		if currentUID == -1 || k.prefix != lastPrefix {
			currentUID++
			uniqueRepresentative = append(uniqueRepresentative, origIdx)
			lastPrefix = k.prefix
			lastStr = s.entries[origIdx]
		} else {
			str := s.entries[origIdx]
			if str != lastStr {
				currentUID++
				uniqueRepresentative = append(uniqueRepresentative, origIdx)
				lastStr = str
			}
		}

		uniqueID[origIdx] = currentUID
	}

	numUnique := int32(len(uniqueRepresentative))

	keys = nil // free

	runtime.GC()

	getUniqString := func(uid int32) string {
		return s.entries[uniqueRepresentative[uid]]
	}

	// suffix sorting
	suffKeys := make([]suffixKey, numUnique)
	chunkSizeUnique := (int(numUnique) + numCPU - 1) / numCPU

	for g := range numCPU {
		start := g * chunkSizeUnique
		end := min(start+chunkSizeUnique, int(numUnique))

		if start >= end {
			continue
		}

		wg.Go(func() {
			for i := start; i < end; i++ {
				uid := int32(i)

				suffKeys[i] = suffixKey{
					uid:    uid,
					suffix: getSuffix64(getUniqString(uid)),
				}
			}
		})
	}

	wg.Wait()

	bucketSortSuffixKeys(suffKeys, uniqueRepresentative, s.entries)

	parent := make([]int32, numUnique)

	for i := range parent {
		parent[i] = -1
	}

	parentOffset := make([]uint8, numUnique)

	// parallel prefix overlapping loops
	for g := range numCPU {
		start := g * chunkSizeUnique
		end := min(start+chunkSizeUnique, int(numUnique)-1)

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
		start := g * chunkSizeUnique
		end := min(start+chunkSizeUnique, int(numUnique)-1)

		if start >= end {
			continue
		}

		wg.Go(func() {
			for i := start; i < end; i++ {
				idxA := suffKeys[i].uid
				idxB := suffKeys[i+1].uid

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

	suffKeys = nil // free

	runtime.GC()

	var blobCap int

	for i := range numUnique {
		if parent[i] == -1 {
			blobCap += len(getUniqString(i))
		}
	}

	// greedy suffix-to-prefix chaining
	getPrefixBucket2 := func(str string) int {
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

	getSuffixBucket2 := func(str string) int {
		ln := len(str)
		if ln == 0 {
			return 0
		}

		if ln == 1 {
			return 1 + int(str[0])
		}

		_ = str[ln-1] // BCE
		_ = str[ln-2]

		return 257 + int(str[ln-2])<<8 + int(str[ln-1])
	}

	head := make([]int32, numPrefixBuckets)

	for i := range head {
		head[i] = -1
	}

	next := make([]int32, numUnique)

	// thread root nodes into buckets based on their 2-byte prefix
	for idx := range numUnique {
		if parent[idx] == -1 {
			str := getUniqString(idx)
			bucket := getPrefixBucket2(str)

			next[idx] = head[bucket]
			head[bucket] = idx
		}
	}

	resolvedOffset := make([]uint32, numUnique)

	for i := range resolvedOffset {
		resolvedOffset[i] = 0xFFFFFFFF
	}

	orderedRoots := make([]int32, 0, numUnique)
	visited := make([]bool, numUnique)

	// greedy hamiltonian path construction over prefix/suffix buckets
	for idx := range numUnique {
		if parent[idx] != -1 || visited[idx] {
			continue
		}

		curr := idx

		for curr != -1 {
			visited[curr] = true
			orderedRoots = append(orderedRoots, curr)

			str := getUniqString(curr)
			bucket := getSuffixBucket2(str)

			nextRoot := int32(-1)
			prevInBucket := int32(-1)
			item := head[bucket]

			// linked-list traversal by pruning visited nodes on the fly
			for item != -1 {
				if visited[item] {
					if prevInBucket == -1 {
						head[bucket] = next[item]
					} else {
						next[prevInBucket] = next[item]
					}

					item = next[item]

					continue
				}

				nextRoot = item

				if prevInBucket == -1 {
					head[bucket] = next[item]
				} else {
					next[prevInBucket] = next[item]
				}

				break
			}

			curr = nextRoot
		}
	}

	blob := make([]byte, 0, blobCap)

	// merge with exact suffix-to-prefix overlap on sequentially ordered roots
	for _, idx := range orderedRoots {
		str := getUniqString(idx)

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

	// resolve child offsets from parents
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

// Bytes returns the raw underlying byte slice of the PackedBlob.
// This slice should not be modified.
func (s *PackedBlob) Bytes() []byte {
	return s.blob
}

// Len returns the total length of the packed byte slice in the blob.
func (s *PackedBlob) Len() int {
	return len(s.blob)
}

// Size returns the total size of the packed bytes and pointers in memory.
func (s *PackedBlob) Size() int {
	return len(s.blob) + len(s.pointers)*int(unsafe.Sizeof(Pointer{}))
}

func getPrefix64(str string) uint64 {
	var p uint64

	ln := len(str)
	if ln >= 8 {
		_ = str[7] // BCE

		p = uint64(str[0])<<56 | uint64(str[1])<<48 | uint64(str[2])<<40 | uint64(str[3])<<32 | uint64(str[4])<<24 | uint64(str[5])<<16 | uint64(str[6])<<8 | uint64(str[7])
	} else if ln > 0 {
		_ = str[0] // BCE

		for j := range ln {
			p |= uint64(str[j]) << (56 - j*8)
		}
	}

	return p
}

func getSuffix64(str string) uint64 {
	var s uint64

	ln := len(str)
	if ln >= 8 {
		_ = str[ln-8] // BCE

		s = uint64(str[ln-1])<<56 | uint64(str[ln-2])<<48 | uint64(str[ln-3])<<40 | uint64(str[ln-4])<<32 | uint64(str[ln-5])<<24 | uint64(str[ln-6])<<16 | uint64(str[ln-7])<<8 | uint64(str[ln-8])
	} else if ln > 0 {
		_ = str[0] // BCE

		for j := range ln {
			s |= uint64(str[ln-1-j]) << (56 - j*8)
		}
	}

	return s
}

func compareSortKey(a, b sortKey, entries []string) int {
	if a.prefix != b.prefix {
		if a.prefix < b.prefix {
			return -1
		}

		return 1
	}

	return cmp.Compare(entries[a.idx], entries[b.idx])
}

func compareSuffixKey(a, b suffixKey, uniqueRepresentative []int32, entries []string) int {
	if a.suffix != b.suffix {
		if a.suffix < b.suffix {
			return -1
		}

		return 1
	}

	strA := entries[uniqueRepresentative[a.uid]]
	strB := entries[uniqueRepresentative[b.uid]]

	return compareReversed(strA, strB)
}

func bucketSortKeys(keys []sortKey, entries []string) {
	length := len(keys)
	if length < 2 {
		return
	}

	const numBuckets = 65793

	getBucket := func(k sortKey) int {
		p := k.prefix
		b1 := byte(p >> 56)
		b2 := byte(p >> 48)

		if b1 == 0 {
			return 0
		}
		if b2 == 0 {
			return 1 + int(b1)
		}
		return 257 + int(b1)<<8 + int(b2)
	}

	counts := make([]int32, numBuckets)
	for i := range keys {
		counts[getBucket(keys[i])]++
	}

	offsets := make([]int32, numBuckets)
	var sum int32
	for i := range counts {
		offsets[i] = sum
		sum += counts[i]
	}

	temp := make([]sortKey, length)
	pos := make([]int32, numBuckets)
	copy(pos, offsets)

	for i := range keys {
		bucket := getBucket(keys[i])
		temp[pos[bucket]] = keys[i]
		pos[bucket]++
	}

	copy(keys, temp)

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
	numCPU := runtime.GOMAXPROCS(0)

	for range numCPU {
		wg.Go(func() {
			for bIdx := range bucketIdxChan {
				start := offsets[bIdx]
				end := start + counts[bIdx]

				slices.SortFunc(keys[start:end], func(a, b sortKey) int {
					return compareSortKey(a, b, entries)
				})
			}
		})
	}

	wg.Wait()
}

func bucketSortSuffixKeys(keys []suffixKey, uniqueRepresentative []int32, entries []string) {
	length := len(keys)
	if length < 2 {
		return
	}

	const numBuckets = 65793

	getBucket := func(k suffixKey) int {
		s := k.suffix
		b1 := byte(s >> 56)
		b2 := byte(s >> 48)

		if b1 == 0 {
			return 0
		}
		if b2 == 0 {
			return 1 + int(b1)
		}
		return 257 + int(b1)<<8 + int(b2)
	}

	counts := make([]int32, numBuckets)
	for i := range keys {
		counts[getBucket(keys[i])]++
	}

	offsets := make([]int32, numBuckets)
	var sum int32
	for i := range counts {
		offsets[i] = sum
		sum += counts[i]
	}

	temp := make([]suffixKey, length)
	pos := make([]int32, numBuckets)
	copy(pos, offsets)

	for i := range keys {
		bucket := getBucket(keys[i])
		temp[pos[bucket]] = keys[i]
		pos[bucket]++
	}

	copy(keys, temp)

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
	numCPU := runtime.GOMAXPROCS(0)

	for range numCPU {
		wg.Go(func() {
			for bIdx := range bucketIdxChan {
				start := offsets[bIdx]
				end := start + counts[bIdx]

				slices.SortFunc(keys[start:end], func(a, b suffixKey) int {
					return compareSuffixKey(a, b, uniqueRepresentative, entries)
				})
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
