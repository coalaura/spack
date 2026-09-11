package spack

import (
	"cmp"
	"errors"
	"math"
	"math/bits"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

const (
	// MaxStringLen is the maximum allowed length of a string to be packed.
	// This limit is constrained because pointer lengths are stored as uint8.
	MaxStringLen = math.MaxUint8

	// numBuckets is the fan-out of the count/scatter partitions. Keys are bucketed
	// by their top 16 bits, so bucket order equals key order.
	numBuckets = 1 << 16

	// maxOverlapLevel is the longest overlap handled by the window-based
	// chaining pass. Longer overlaps use the root index.
	maxOverlapLevel = 8

	// packedKeyLevel is the highest level whose k-byte key fits next to a root
	// index in one uint64 (16 bucket bits + 32 key bits + 32 index bits). Higher
	// levels resolve the remaining key bytes through the suffix window.
	packedKeyLevel = 6

	// forEachBucketBatch is the number of buckets each forEachBucket worker
	// claims per turn. Small batches distribute skewed prefix buckets better.
	forEachBucketBatch = 1
)

// PackOptions configures a Pack operation.
type PackOptions struct {
	// DisableGC skips explicit garbage collections during packing. The Go
	// runtime may still perform automatic garbage collection.
	DisableGC bool
}

// PackedBlob holds a single concatenated slice of bytes containing all packed
// strings. T selects the offset width used by its pointers.
type PackedBlob[T PointerType] struct {
	pointers []T
	blob     []byte
}

var (
	ErrTooManyStrings = errors.New("too many strings to pack")
	ErrBlobTooLarge   = errors.New("packed blob exceeds pointer offset range")
)

// Pack compresses all strings currently collected in the StringMap into a PackedBlob.
func (s *StringMap) Pack[T PointerType](options ...PackOptions) (*PackedBlob[T], error) {
	return s.pack[T](nil, options...)
}

// PackWithBlobSizeBound packs the strings and additionally calculates a
// certified size bound for the final deduplicated, containment-reduced roots.
func (s *StringMap) PackWithBlobSizeBound[T PointerType](bound *BlobSizeBound, options ...PackOptions) (*PackedBlob[T], error) {
	return s.pack[T](bound, options...)
}

func (s *StringMap) pack[T PointerType](requestedBound *BlobSizeBound, options ...PackOptions) (*PackedBlob[T], error) {
	s.mx.RLock()
	defer s.mx.RUnlock()

	var disableGC bool

	for _, option := range options {
		disableGC = disableGC || option.DisableGC
	}

	entries := s.entries

	length := len(entries)
	if length == 0 {
		if requestedBound != nil {
			*requestedBound = BlobSizeBound{}
		}

		return &PackedBlob[T]{}, nil
	}

	if length > math.MaxInt32 {
		return nil, ErrTooManyStrings
	}

	numCPU := runtime.GOMAXPROCS(0)

	var tooLong atomic.Bool

	parallelFor(length, numCPU, func(start, end int) {
		for i := start; i < end; i++ {
			if len(entries[i]) > MaxStringLen {
				tooLong.Store(true)

				return
			}
		}
	})

	if tooLong.Load() {
		return nil, ErrStringTooLong
	}

	// The bucket supplies the first two prefix bytes. Cache the next four
	// beside the input index, giving six cached bytes in an eight-byte word.
	var keys []uint64

	offsets := bucketPartition(
		length, numCPU,
		func(i int) int {
			return int(getPrefix64(entries[i]) >> 48)
		},
		func(total int) {
			keys = make([]uint64, total)
		},
		func(i int, pos int32) {
			keys[pos] = getPrefix64(entries[i])<<16&highMask(4) | uint64(uint32(i))
		},
	)

	sortPackedBuckets(keys, offsets, numCPU, func(a, b uint64) int {
		return compareSortWord(a, b, entries)
	})

	chunkSize, chunkBase := markUniqueWords(keys, entries, numCPU)
	numUnique := chunkBase[len(chunkBase)-1]

	var (
		uniqueIDs            = make([]int32, length)
		uniqueRepresentative = make([]int32, numUnique)
		uPrefix              = make([]uint64, numUnique)
		uLen                 = make([]uint8, numUnique)
	)

	parallelChunks(length, chunkSize, func(c, start, end int) {
		uid := chunkBase[c] - 1

		for i := start; i < end; i++ {
			word := keys[i]

			input := uint32(word) & sortWordIndexMask
			str := entries[input]

			if word&sortWordUnique != 0 {
				uid++

				uniqueRepresentative[uid] = int32(input)
				uPrefix[uid] = getPrefix64(str)
				uLen[uid] = uint8(len(str))
			}

			uniqueIDs[input] = uid
		}
	})

	keys = nil

	// This releases an 8*N-byte allocation before the per-unique phases.
	if !disableGC {
		runtime.GC()
	}

	uniqString := func(uid int32) string {
		return entries[uniqueRepresentative[uid]]
	}

	uSuffixWin := make([]uint64, numUnique)

	parallelFor(int(numUnique), numCPU, func(start, end int) {
		for i := start; i < end; i++ {
			uSuffixWin[i] = getSuffixWindow64(uniqString(int32(i)))
		}
	})

	var suffKeys []uint64

	offsets = bucketPartition(
		int(numUnique), numCPU,
		func(i int) int {
			return int(bits.ReverseBytes64(uSuffixWin[i]) >> 48)
		},
		func(total int) {
			suffKeys = make([]uint64, total)
		},
		func(i int, pos int32) {
			rev := bits.ReverseBytes64(uSuffixWin[i])

			suffKeys[pos] = uint64(uint32(rev>>16))<<32 | uint64(uint32(i))
		},
	)

	sortPackedBuckets(suffKeys, offsets, numCPU, func(a, b uint64) int {
		if a>>32 != b>>32 {
			return cmp.Compare(a>>32, b>>32)
		}

		uidA := int32(uint32(a))
		uidB := int32(uint32(b))

		if uLen[uidA] <= 6 && uLen[uidB] <= 6 {
			return cmp.Compare(uLen[uidA], uLen[uidB])
		}

		return compareReversed(uniqString(uidA), uniqString(uidB))
	})

	parent := make([]int32, numUnique)

	for i := range parent {
		parent[i] = -1
	}

	parentOffset := make([]uint8, numUnique)

	parallelFor(int(numUnique)-1, numCPU, func(start, end int) {
		for i := start; i < end; i++ {
			idxA := int32(i)
			idxB := int32(i + 1)

			lenA := int(uLen[idxA])
			if lenA > int(uLen[idxB]) {
				continue
			}

			mask := highMask(min(lenA, 8))
			if uPrefix[idxA]&mask != uPrefix[idxB]&mask {
				continue
			}

			if lenA <= 8 || strings.HasPrefix(uniqString(idxB), uniqString(idxA)) {
				parent[idxA] = idxB
			}
		}
	})

	parallelFor(int(numUnique)-1, numCPU, func(start, end int) {
		for i := start; i < end; i++ {
			idxA := int32(uint32(suffKeys[i]))
			idxB := int32(uint32(suffKeys[i+1]))

			if parent[idxA] != -1 {
				continue
			}

			lenA := int(uLen[idxA])
			lenB := int(uLen[idxB])

			if lenA > lenB {
				continue
			}

			mask := lowMask(min(lenA, 8))
			if uSuffixWin[idxA]&mask != uSuffixWin[idxB]&mask {
				continue
			}

			if lenA <= 8 || strings.HasSuffix(uniqString(idxB), uniqString(idxA)) {
				parent[idxA] = idxB
				parentOffset[idxA] = uint8(lenB - lenA)
			}
		}
	})

	suffKeys = nil

	// Release the reversed-sort words before building the root indexes.
	if !disableGC {
		runtime.GC()
	}

	var numRoots int

	for _, p := range parent {
		if p == -1 {
			numRoots++
		}
	}

	roots := make([]int32, 0, numRoots)

	for uid, p := range parent {
		if p == -1 {
			roots = append(roots, int32(uid))
		}
	}

	rLen := make([]uint8, numRoots)

	for j, uid := range roots {
		uPrefix[j] = uPrefix[uid]
		uSuffixWin[j] = uSuffixWin[uid]
		rLen[j] = uLen[uid]
	}

	rPrefix := uPrefix[:numRoots]
	rSuffix := uSuffixWin[:numRoots]

	uPrefix = nil
	uSuffixWin = nil
	uLen = nil

	rCandidates := removeInternalContainment(
		entries, uniqueRepresentative, roots, rPrefix, rLen,
		parent, parentOffset, numCPU,
	)

	// Removing roots preserves lexicographic order. Compact all root-indexed
	// arrays together; these are temporary arrays, not returned storage.
	numRoots = 0

	for j, uid := range roots {
		if parent[uid] != -1 {
			continue
		}

		roots[numRoots] = uid
		rPrefix[numRoots] = rPrefix[j]
		rSuffix[numRoots] = rSuffix[j]
		rLen[numRoots] = rLen[j]
		rCandidates[numRoots] = rCandidates[j]

		numRoots++
	}

	roots = roots[:numRoots]
	rPrefix = rPrefix[:numRoots]
	rSuffix = rSuffix[:numRoots]
	rLen = rLen[:numRoots]
	rCandidates = rCandidates[:numRoots]

	chains := chainRoots(
		entries, uniqueRepresentative, roots,
		rPrefix, rSuffix, rLen, rCandidates, numCPU,
	)

	refineSmallRootSet(entries, uniqueRepresentative, roots, chains)

	rCandidates = nil

	resolvedOffset := make([]uint64, numUnique)

	blobLen, err := planSubstringFreeBlob(
		entries, uniqueRepresentative, roots, rLen,
		chains, resolvedOffset, numCPU,
	)
	if err != nil {
		return nil, err
	}

	if requestedBound != nil {
		bound, boundErr := calculateBlobSizeBound(entries, uniqueRepresentative, roots, rPrefix, rLen)
		if boundErr != nil {
			return nil, boundErr
		}

		bound.CurrentBlobBytes = uint64(blobLen)

		*requestedBound = bound
	}

	// Every parent is strictly longer than its child. Consequently a path
	// contains at most 255 edges, even when empty strings are present.
	for uid := range numUnique {
		curr := uid

		var (
			path  [MaxStringLen + 1]int32
			depth int
		)

		for parent[curr] != -1 {
			path[depth] = curr
			depth++

			curr = parent[curr]
		}

		baseOffset := resolvedOffset[curr]

		for j := depth - 1; j >= 0; j-- {
			node := path[j]

			baseOffset += uint64(parentOffset[node])
			resolvedOffset[node] = baseOffset

			// Path compression also marks this offset as resolved; no
			// offset needs to be reserved as a sentinel.
			parent[node] = -1
		}
	}

	maxOffset := pointerMaxOffset[T]()

	for _, offset := range resolvedOffset {
		if offset > maxOffset {
			return nil, ErrBlobTooLarge
		}
	}

	pointers := makePointers[T](entries, uniqueIDs, resolvedOffset, numCPU)

	parent = nil
	parentOffset = nil
	resolvedOffset = nil
	rPrefix = nil
	rSuffix = nil
	rLen = nil

	// The final allocation happens only after its exact length is known.
	if !disableGC {
		runtime.GC()
	}

	blob := make([]byte, blobLen)

	var pos int

	for start := range roots {
		if chains.hasPred[start] {
			continue
		}

		for curr := int32(start); curr != -1; curr = chains.succ[curr] {
			str := entries[uniqueRepresentative[roots[curr]]]

			pos += copy(blob[pos:], str[int(chains.overlap[curr]):])
		}
	}

	return &PackedBlob[T]{
		pointers: pointers,
		blob:     blob,
	}, nil
}

// GetStringUnsafe returns a zero-copy string pointing directly into the blob's memory.
// It is fast but unsafe: the returned string's lifetime is tied to the blob,
// and it will reflect any future modifications made to the underlying slice.
func (s *PackedBlob[T]) GetStringUnsafe(pointer T) (string, error) {
	return GetStringUnsafe(s.blob, pointer)
}

// GetString returns a copied, independent string from the PackedBlob.
// It allocates a new underlying buffer to ensure the returned string can
// safely outlive the blob and remains isolated from any future mutations.
func (s *PackedBlob[T]) GetString(pointer T) (string, error) {
	return GetString(s.blob, pointer)
}

// Pointers returns all pointers.
func (s *PackedBlob[T]) Pointers() []T {
	return s.pointers
}

// Bytes returns the raw underlying byte slice of the PackedBlob.
// This slice should not be modified.
func (s *PackedBlob[T]) Bytes() []byte {
	return s.blob
}

// Len returns the total length of the packed byte slice in the blob.
func (s *PackedBlob[T]) Len() int {
	return len(s.blob)
}

// Size returns the total size of the packed bytes and pointers in memory.
func (s *PackedBlob[T]) Size() int {
	return len(s.blob) + len(s.pointers)*int(unsafe.Sizeof(*new(T)))
}

func makePointers[T PointerType](entries []string, uniqueIDs []int32, resolvedOffset []uint64, numCPU int) []T {
	switch any(*new(T)).(type) {
	case Pointer16:
		pointers := make([]Pointer16, len(uniqueIDs))

		parallelFor(len(pointers), numCPU, func(start, end int) {
			for i := start; i < end; i++ {
				pointers[i] = NewPointer16(uint16(resolvedOffset[uniqueIDs[i]]), uint8(len(entries[i])))
			}
		})

		return any(pointers).([]T)
	case Pointer32:
		pointers := make([]Pointer32, len(uniqueIDs))

		parallelFor(len(pointers), numCPU, func(start, end int) {
			for i := start; i < end; i++ {
				pointers[i] = NewPointer32(uint32(resolvedOffset[uniqueIDs[i]]), uint8(len(entries[i])))
			}
		})

		return any(pointers).([]T)
	case Pointer64:
		pointers := make([]Pointer64, len(uniqueIDs))

		parallelFor(len(pointers), numCPU, func(start, end int) {
			for i := start; i < end; i++ {
				pointers[i] = NewPointer64(resolvedOffset[uniqueIDs[i]], uint8(len(entries[i])))
			}
		})

		return any(pointers).([]T)
	default:
		panic("unsupported pointer type")
	}
}

func pointerMaxOffset[T PointerType]() uint64 {
	switch any(*new(T)).(type) {
	case Pointer16:
		return math.MaxUint16
	case Pointer32:
		return math.MaxUint32
	case Pointer64:
		return math.MaxUint64
	default:
		panic("unsupported pointer type")
	}
}

// parallelFor runs body over [0, n) split into numCPU contiguous chunks.
func parallelFor(n, numCPU int, body func(start, end int)) {
	if n <= 0 {
		return
	}

	parallelChunks(n, 1+(n-1)/numCPU, func(_, start, end int) {
		body(start, end)
	})
}

// parallelChunks runs body over [0, n) in contiguous chunks of chunkSize items,
// passing the chunk index so callers can combine per-chunk results.
func parallelChunks(n, chunkSize int, body func(chunk, start, end int)) {
	if n <= 0 || chunkSize <= 0 {
		return
	}

	var wg sync.WaitGroup

	for c := range 1 + (n-1)/chunkSize {
		start := c * chunkSize
		end := start + min(chunkSize, n-start)

		wg.Go(func() {
			body(c, start, end)
		})
	}

	wg.Wait()
}

// bucketPartition distributes n items into numBuckets buckets with two parallel
// passes (count, scatter). bucketOf returns the bucket of item i or -1 to skip
// it; alloc is called once with the number of kept items before scattering;
// place stores item i at its final position. Items keep their relative order
// inside a bucket. The returned offsets have length numBuckets+1.
func bucketPartition(n, numCPU int, bucketOf func(i int) int, alloc func(total int), place func(i int, pos int32)) []int32 {
	offsets := make([]int32, numBuckets+1)

	if n <= 0 {
		alloc(0)

		return offsets
	}

	chunk := 1 + (n-1)/numCPU
	numChunks := 1 + (n-1)/chunk

	counts := make([]int32, numChunks*numBuckets)

	var wg sync.WaitGroup

	for c := range numChunks {
		start := c * chunk
		end := start + min(chunk, n-start)

		cnt := counts[c*numBuckets : (c+1)*numBuckets]

		wg.Go(func() {
			for i := start; i < end; i++ {
				b := bucketOf(i)

				if b >= 0 {
					cnt[b]++
				}
			}
		})
	}

	wg.Wait()

	// bucket-major, chunk-minor prefix sums; counts becomes per-chunk cursors
	var sum int32

	for b := range numBuckets {
		offsets[b] = sum

		for c := range numChunks {
			cnt := &counts[c*numBuckets+b]

			next := sum + *cnt

			*cnt = sum
			sum = next
		}
	}

	offsets[numBuckets] = sum

	alloc(int(sum))

	for c := range numChunks {
		start := c * chunk
		end := start + min(chunk, n-start)

		pos := counts[c*numBuckets : (c+1)*numBuckets]

		wg.Go(func() {
			for i := start; i < end; i++ {
				b := bucketOf(i)

				if b >= 0 {
					place(i, pos[b])

					pos[b]++
				}
			}
		})
	}

	wg.Wait()

	return offsets
}

// sortBuckets sorts every bucket of items independently and in parallel.
func sortBuckets[E any](items []E, offsets []int32, numCPU int, compare func(a, b E) int) {
	forEachBucket(numCPU, func(b int) {
		start := offsets[b]
		end := offsets[b+1]

		if end-start >= 2 {
			slices.SortFunc(items[start:end], compare)
		}
	})
}

// sortBucketsOrdered is sortBuckets for naturally ordered items; slices.Sort
// avoids the comparator call per comparison.
func sortBucketsOrdered[E cmp.Ordered](items []E, offsets []int32, numCPU int) {
	forEachBucket(numCPU, func(b int) {
		start := offsets[b]
		end := offsets[b+1]

		if end-start >= 2 {
			slices.Sort(items[start:end])
		}
	})
}

// forEachBucket calls body for every bucket index, distributing batches of
// buckets over numCPU workers. Bodies must only touch their own bucket.
func forEachBucket(numCPU int, body func(bucket int)) {
	var (
		nextBucket atomic.Int32
		wg         sync.WaitGroup
	)

	for range numCPU {
		wg.Go(func() {
			for {
				first := int(nextBucket.Add(forEachBucketBatch)) - forEachBucketBatch
				if first >= numBuckets {
					return
				}

				for b := first; b < min(first+forEachBucketBatch, numBuckets); b++ {
					body(b)
				}
			}
		})
	}

	wg.Wait()
}

// getPrefix64 returns the first 8 bytes big-endian, zero padded on the right.
func getPrefix64(str string) uint64 {
	if len(str) >= 8 {
		_ = str[7] // BCE

		return uint64(str[0])<<56 | uint64(str[1])<<48 | uint64(str[2])<<40 | uint64(str[3])<<32 |
			uint64(str[4])<<24 | uint64(str[5])<<16 | uint64(str[6])<<8 | uint64(str[7])
	}

	var p uint64

	for i := 0; i < len(str); i++ {
		p |= uint64(str[i]) << (56 - 8*uint(i))
	}

	return p
}

// getSuffixWindow64 returns the last 8 bytes big-endian (last byte lowest),
// zero padded on the left. Shifting it left by 64-8k yields the exact k-byte
// suffix key; bits.ReverseBytes64 yields the reversed-order sort key.
func getSuffixWindow64(str string) uint64 {
	ln := len(str)

	if ln >= 8 {
		str = str[ln-8:]

		return uint64(str[0])<<56 | uint64(str[1])<<48 | uint64(str[2])<<40 | uint64(str[3])<<32 |
			uint64(str[4])<<24 | uint64(str[5])<<16 | uint64(str[6])<<8 | uint64(str[7])
	}

	var w uint64

	for i := range ln {
		w = w<<8 | uint64(str[i])
	}

	return w
}

// highMask keeps the top n bytes of a prefix key, n in [0, 8].
func highMask(n int) uint64 {
	return ^uint64(0) << (64 - 8*uint(n))
}

// lowMask keeps the low n bytes of a suffix window, n in [0, 8].
func lowMask(n int) uint64 {
	if n >= 8 {
		return ^uint64(0)
	}

	return 1<<(8*uint(n)) - 1
}

func compareReversed(s1, s2 string) int {
	n1 := len(s1)
	n2 := len(s2)

	minLen := min(n2, n1)

	if minLen > 0 {
		// BCE
		_ = s1[n1-minLen]
		_ = s2[n2-minLen]

		for i := 1; i <= minLen; i++ {
			c1 := s1[n1-i]
			c2 := s2[n2-i]

			if c1 != c2 {
				return cmp.Compare(c1, c2)
			}
		}
	}

	return cmp.Compare(n1, n2)
}
