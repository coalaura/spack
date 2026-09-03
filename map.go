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
	// This limit is constrained because the Pointer.Length field is a uint8.
	MaxStringLen = math.MaxUint8

	// numBuckets is the fan-out of the count/scatter partitions. Keys are bucketed
	// by their top 16 bits, so bucket order equals key order.
	numBuckets = 1 << 16

	// maxOverlapLevel is the longest suffix-to-prefix overlap the greedy chaining
	// matches exactly; bounded by the 8-byte prefix/suffix windows.
	maxOverlapLevel = 8

	// packedKeyLevel is the highest level whose k-byte key fits next to a root
	// index in one uint64 (16 bucket bits + 32 key bits + 32 index bits). Higher
	// levels resolve the remaining key bytes through the suffix window.
	packedKeyLevel = 6

	// forEachBucketBatch is the number of buckets each forEachBucket worker
	// claims per turn.
	forEachBucketBatch = 256
)

var (
	ErrTooManyStrings = errors.New("too many strings to pack")
	ErrBlobTooLarge   = errors.New("packed blob exceeds uint32 offset range")
)

// PackedBlob holds a single concatenated slice of bytes containing
// all the packed strings. Strings are retrieved using a Pointer.
type PackedBlob struct {
	pointers []Pointer
	blob     []byte
}

type sortKey struct {
	prefix uint64 // first 8 bytes, big-endian, zero padded
	idx    int32
	ln     uint8
}

// Pack compresses all strings currently collected in the StringMap into a PackedBlob.
func (s *StringMap) Pack() (*PackedBlob, error) {
	s.mx.RLock()
	defer s.mx.RUnlock()

	entries := s.entries

	length := len(entries)
	if length == 0 {
		return &PackedBlob{}, nil
	}

	if length > math.MaxInt32 {
		return nil, ErrTooManyStrings
	}

	numCPU := runtime.GOMAXPROCS(0)

	// NewStringMap accepts unvalidated entries, so lengths must be checked here.
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

	// lexicographic sort: 16-bit bucket on the prefix, full compare inside buckets.
	// Scattering straight from entries avoids a second keys-sized temporary.
	var keys []sortKey

	offsets := bucketPartition(
		length, numCPU,
		func(i int) int {
			return int(getPrefix64(entries[i]) >> 48)
		},
		func(total int) {
			keys = make([]sortKey, total)
		},
		func(i int, pos int32) {
			str := entries[i]

			keys[pos] = sortKey{
				prefix: getPrefix64(str),
				idx:    int32(i),
				ln:     uint8(len(str)),
			}
		},
	)

	sortBuckets(keys, offsets, numCPU, func(a, b sortKey) int {
		return compareSortKey(a, b, entries)
	})

	// deduplicate: count uniques first so the per-unique arrays are allocated
	// exactly once (no append regrowth garbage while keys is still live).
	// pointers temporarily carry the unique id in their offset field.
	startsUnique := func(i int) bool {
		if i == 0 {
			return true
		}

		prev := keys[i-1]
		cur := keys[i]

		if prev.prefix != cur.prefix || prev.ln != cur.ln {
			return true
		}

		// equal prefix and length settle strings up to 8 bytes without touching them
		return cur.ln > 8 && entries[prev.idx] != entries[cur.idx]
	}

	chunkSize := (length + numCPU - 1) / numCPU
	numChunks := (length + chunkSize - 1) / chunkSize

	chunkBase := make([]int32, numChunks+1)

	parallelChunks(length, chunkSize, func(c, start, end int) {
		var count int32

		for i := start; i < end; i++ {
			if startsUnique(i) {
				count++
			}
		}

		chunkBase[c+1] = count
	})

	for c := range numChunks {
		chunkBase[c+1] += chunkBase[c]
	}

	numUnique := chunkBase[numChunks]

	var (
		pointers             = make([]Pointer, length)
		uniqueRepresentative = make([]int32, numUnique)
		uPrefix              = make([]uint64, numUnique)
		uLen                 = make([]uint8, numUnique)
	)

	parallelChunks(length, chunkSize, func(c, start, end int) {
		uid := chunkBase[c] - 1

		for i := start; i < end; i++ {
			k := keys[i]

			if startsUnique(i) {
				uid++

				uniqueRepresentative[uid] = k.idx
				uPrefix[uid] = k.prefix
				uLen[uid] = k.ln
			}

			pointers[k.idx] = NewPointer(uint32(uid), 0)
		}
	})

	keys = nil // free the largest temporary before the next allocations

	runtime.GC()

	uniqString := func(uid int32) string {
		return entries[uniqueRepresentative[uid]]
	}

	// suffix windows (last 8 bytes, forward order) feed the reversed sort,
	// suffix containment and chaining without further string accesses
	uSuffixWin := make([]uint64, numUnique)

	parallelFor(int(numUnique), numCPU, func(start, end int) {
		for i := start; i < end; i++ {
			uSuffixWin[i] = getSuffixWindow64(uniqString(int32(i)))
		}
	})

	// reversed order: 16-bit bucket on the last two bytes, the next four
	// reversed bytes packed above the uid, remaining ties settled on the strings
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

	sortBuckets(suffKeys, offsets, numCPU, func(a, b uint64) int {
		if a>>32 != b>>32 {
			return cmp.Compare(a>>32, b>>32)
		}

		uidA := int32(uint32(a))
		uidB := int32(uint32(b))

		// equal 6-byte reversed key and both within it: the shorter is a suffix of the longer
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

	// prefix containment: in lexicographic order a string that is a prefix of
	// any later string is a prefix of its immediate successor
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

	// suffix containment: same argument in reversed order
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

	suffKeys = nil // free

	runtime.GC()

	// roots (parent == -1) in uid order. Chaining works on root indices; the
	// windows are compacted in place (j <= roots[j], so reads stay ahead of
	// writes). The compacted slices keep their per-unique backing arrays,
	// which trades ~16 B per non-root for one fewer full GC cycle.
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

	// roots are in lexicographic order, so the roots whose prefix falls into
	// one 16-bit bucket form a contiguous range
	rootBucketStart := make([]int32, numBuckets+1)

	for _, p := range rPrefix {
		rootBucketStart[int(p>>48)+1]++
	}

	for b := range numBuckets {
		rootBucketStart[b+1] += rootBucketStart[b]
	}

	// greedy suffix-to-prefix chaining, longest exact overlap first.
	// rSucc links a chain tail to the next root; rOther maps a chain head to its
	// tail and vice versa (cycle check); rHasPred marks linked heads.
	rSucc := make([]int32, numRoots)
	rOther := make([]int32, numRoots)

	for j := range rSucc {
		rSucc[j] = -1
		rOther[j] = int32(j)
	}

	rHasPred := make([]bool, numRoots)
	rOverlap := make([]uint8, numRoots)

	// tail words: 32 key bits below the bucket in the high half, root index in
	// the low half; sized once so no level regrows it
	tails := make([]uint64, numRoots)

	for k := maxOverlapLevel; k >= 1; k-- {
		shift := uint(64 - 8*k)

		mask := highMask(k)

		// exact k-byte key of a tail word; levels above packedKeyLevel have key
		// bytes beyond the packed 48 bits and read them from the suffix window
		tailKey := func(word uint64, bucket int) uint64 {
			if k <= packedKeyLevel {
				return uint64(bucket)<<48 | (word>>32)<<16
			}

			return rSuffix[uint32(word)] << shift
		}

		// tails: free chain ends with at least k bytes, bucketed by the key
		tailOffsets := bucketPartition(
			numRoots, numCPU,
			func(j int) int {
				if rSucc[j] != -1 || int(rLen[j]) < k {
					return -1
				}

				return int(rSuffix[j] << shift >> 48)
			},
			func(total int) {
				tails = tails[:total]
			},
			func(j int, pos int32) {
				tails[pos] = uint64(uint32(rSuffix[j]<<shift<<16>>32))<<32 | uint64(uint32(j))
			},
		)

		// for k <= 2 the bucket already is the whole key
		switch {
		case k <= 2:
		case k <= packedKeyLevel:
			sortBucketsOrdered(tails, tailOffsets, numCPU)
		default:
			sortBuckets(tails, tailOffsets, numCPU, func(a, b uint64) int {
				if a>>32 != b>>32 {
					return cmp.Compare(a>>32, b>>32)
				}

				ka := rSuffix[uint32(a)] << shift
				kb := rSuffix[uint32(b)] << shift

				if ka != kb {
					return cmp.Compare(ka, kb)
				}

				return cmp.Compare(uint32(a), uint32(b))
			})
		}

		// heads: free chain starts in the bucket's root range, merge-joined with
		// the sorted tails. Buckets hold disjoint keys and disjoint root ranges,
		// so every head is offered to at most one tail.
		forEachBucket(numCPU, func(b int) {
			bucketTails := tails[tailOffsets[b]:tailOffsets[b+1]]
			if len(bucketTails) == 0 {
				return
			}

			// a one-byte key spans all 256 prefix buckets sharing its high byte
			hEnd := b + 1

			if k == 1 {
				hEnd = b + 256
			}

			h := rootBucketStart[b]
			hHi := rootBucketStart[hEnd]

			for t := 0; t < len(bucketTails); {
				key := tailKey(bucketTails[t], b)

				tEnd := t + 1

				for tEnd < len(bucketTails) && tailKey(bucketTails[tEnd], b) == key {
					tEnd++
				}

				for ; t < tEnd; t++ {
					for h < hHi && (rHasPred[h] || int(rLen[h]) < k || rPrefix[h]&mask < key) {
						h++
					}

					head := int32(-1)

					if h < hHi && rPrefix[h]&mask == key {
						head = h
						h++
					}

					// the key is no longer needed; park the pairing (head+1, 0 = none) in its place
					bucketTails[t] = uint64(uint32(head+1))<<32 | uint64(uint32(bucketTails[t]))
				}
			}
		})

		// link sequentially; rOther[tail] == head means the pair would close a
		// cycle, in which case both stay free for the lower levels
		for _, word := range tails {
			head := int32(uint32(word>>32)) - 1
			if head < 0 {
				continue
			}

			tail := int32(uint32(word))
			if rOther[tail] == head {
				continue
			}

			rSucc[tail] = head
			rHasPred[head] = true
			rOverlap[head] = uint8(k)

			chainHead := rOther[tail]
			chainTail := rOther[head]

			rOther[chainHead] = chainTail
			rOther[chainTail] = chainHead
		}
	}

	var blobCap int

	for j := range numRoots {
		blobCap += int(rLen[j]) - int(rOverlap[j])
	}

	// free
	rPrefix = nil
	rSuffix = nil
	rOther = nil
	tails = nil
	rLen = nil

	runtime.GC()

	// Every linked root saves at least rOverlap bytes, so blobCap is an upper
	// bound; only incidental longer overlaps found below leave slack.
	blob := make([]byte, 0, blobCap)

	resolvedOffset := make([]uint32, numUnique)

	for i := range resolvedOffset {
		resolvedOffset[i] = math.MaxUint32
	}

	for start := range numRoots {
		if rHasPred[start] {
			continue
		}

		for curr := int32(start); curr != -1; curr = rSucc[curr] {
			uid := roots[curr]
			str := uniqString(uid)

			overlap := findOverlap(blob, str, int(rOverlap[curr]))

			resolvedOffset[uid] = uint32(len(blob) - overlap)

			blob = append(blob, str[overlap:]...)
		}
	}

	if uint64(len(blob)) > math.MaxUint32 {
		return nil, ErrBlobTooLarge
	}

	// resolve child offsets from parents
	for i := range numUnique {
		curr := i

		var (
			path  [256]int32
			depth int
		)

		for curr != -1 && resolvedOffset[curr] == math.MaxUint32 {
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

	// replace the parked unique ids with final offsets
	parallelFor(length, numCPU, func(start, end int) {
		for i := start; i < end; i++ {
			uid := pointers[i].Offset()

			pointers[i] = NewPointer(resolvedOffset[uid], uLen[uid])
		}
	})

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

// parallelFor runs body over [0, n) split into numCPU contiguous chunks.
func parallelFor(n, numCPU int, body func(start, end int)) {
	if n <= 0 {
		return
	}

	parallelChunks(n, (n+numCPU-1)/numCPU, func(_, start, end int) {
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

	for c := 0; c*chunkSize < n; c++ {
		start := c * chunkSize
		end := min(start+chunkSize, n)

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

	chunk := (n + numCPU - 1) / numCPU
	numChunks := (n + chunk - 1) / chunk

	counts := make([]int32, numChunks*numBuckets)

	var wg sync.WaitGroup

	for c := range numChunks {
		start := c * chunk
		end := min((c+1)*chunk, n)

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
		end := min((c+1)*chunk, n)

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

// findOverlap returns the longest k such that the last k bytes of blob equal
// str[:k]. known is a lower bound the caller has already verified.
func findOverlap(blob []byte, str string, known int) int {
	maxOverlap := min(len(str), len(blob))
	if maxOverlap <= known {
		return known
	}

	tail := blob[len(blob)-maxOverlap:]

	_ = tail[maxOverlap-1] // BCE

	first := str[0]
	last := tail[maxOverlap-1]

	for k := maxOverlap; k > known; k-- {
		if tail[maxOverlap-k] == first && str[k-1] == last {
			if string(tail[maxOverlap-k:]) == str[:k] {
				return k
			}
		}
	}

	return known
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

func compareSortKey(a, b sortKey, entries []string) int {
	if a.prefix != b.prefix {
		return cmp.Compare(a.prefix, b.prefix)
	}

	if a.ln <= 8 && b.ln <= 8 {
		return cmp.Compare(a.ln, b.ln)
	}

	return cmp.Compare(entries[a.idx], entries[b.idx])
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
