package spack

import (
	"cmp"
	"math/bits"
	"strings"
	"sync/atomic"
)

const (
	radixTargetSize = 32
	radixMinSize    = 64
	radixMaxBits    = 16
)

// rootIndex indexes a lexicographically sorted, prefix-free set of strings.
// It stores no per-string Go objects and no copies of the string payload.
//
// The Bloom filter indexes eight-byte prefixes, not entire strings. It can
// only reject impossible matches; every possible match is checked exactly.
// Short strings have a separate index because zero padding is not a wildcard.
type rootIndex struct {
	entries         []string
	representatives []int32
	roots           []int32
	prefix          []uint64
	length          []uint8

	buckets      []int32
	short        []int32
	shortKeys    []uint64
	shortBuckets []int32
	shortTwo     []int32
	shortThird   []uint64
	single       [256]int32
	bloom        []uint64
	minLen       int

	radixBits   []uint8
	radixBase   []int32
	radixStarts []int32
}

type rootChains struct {
	succ    []int32
	other   []int32
	hasPred []bool
	overlap []uint8
}

func (r *rootIndex) str(root int32) string {
	return r.entries[r.representatives[r.roots[root]]]
}

func (r *rootIndex) mayHavePrefix(key uint64) bool {
	if len(r.bloom) == 0 {
		return false
	}

	hash := prefixHash(key)
	mask := prefixBits(hash)

	return r.bloom[hash&uint64(len(r.bloom)-1)]&mask == mask
}

// containedAndOverlap shares one dictionary search between containment and
// long-overlap discovery. In a prefix-free dictionary, a contained root is
// the predecessor of suffix, while an overlapping head is its successor.
//
// A contained root rules out an overlapping head: that root would otherwise
// also be a prefix of the head. forbidden excludes the host itself.
func (r *rootIndex) containedAndOverlap(suffix string, forbidden int32, wantOverlap bool) (int32, bool) {
	if len(suffix) == 0 {
		return -1, false
	}

	root := r.single[suffix[0]]
	if root != -1 {
		return root, false
	}

	key := getPrefix64(suffix)

	root = r.shortContained(suffix, key)
	if root != -1 {
		return root, false
	}

	if len(suffix) < 8 || !r.mayHavePrefix(key) {
		return -1, false
	}

	start, high := r.prefixRange(key)
	low := start

	// Find the first root strictly greater than suffix. Equality belongs
	// to the containment check, not the proper-overlap check.
	for low < high {
		mid := low + (high-low)/2
		midKey := r.prefix[mid]

		if midKey < key || midKey == key && r.str(mid) <= suffix {
			low = mid + 1
		} else {
			high = mid
		}
	}

	if low > start {
		root := low - 1

		if r.length[root] >= 8 && r.prefix[root] == key &&
			strings.HasPrefix(suffix, r.str(root)) {
			return root, false
		}
	}

	if !wantOverlap || len(suffix) <= maxOverlapLevel {
		return -1, false
	}

	head := low

	if head == forbidden {
		head++
	}

	if int(head) >= len(r.roots) || r.prefix[head] != key {
		return -1, false
	}

	return -1, strings.HasPrefix(r.str(head), suffix)
}

// buildRadix subdivides large two-byte buckets using up to sixteen more
// prefix bits. Each directory aims for 32 roots per interval under a uniform
// distribution; skew may leave larger intervals, but never affects correctness.
func (r *rootIndex) buildRadix() {
	r.radixBits = make([]uint8, numBuckets)
	r.radixBase = make([]int32, numBuckets)

	var total int

	for bucket := range numBuckets {
		count := int(r.buckets[bucket+1] - r.buckets[bucket])
		if count <= radixMinSize {
			continue
		}

		width := min(bits.Len32(uint32((count-1)/radixTargetSize)), radixMaxBits)
		slots := 1 << uint(width)

		r.radixBits[bucket] = uint8(width)
		r.radixBase[bucket] = int32(total)

		total += slots + 1
	}

	r.radixStarts = make([]int32, total)

	for bucket := range numBuckets {
		width := r.radixBits[bucket]
		if width == 0 {
			continue
		}

		slots := 1 << uint(width)
		shift := uint(48 - width)
		mask := uint64(slots - 1)

		base := int(r.radixBase[bucket])
		starts := r.radixStarts[base : base+slots+1]

		start := r.buckets[bucket]
		end := r.buckets[bucket+1]
		cursor := 0

		// Prefixes are sorted. Fill interval boundaries in one forward
		// pass, including empty intervals, without count/scatter scratch.
		for root := start; root < end; root++ {
			slot := int(r.prefix[root] >> shift & mask)

			for cursor <= slot {
				starts[cursor] = root
				cursor++
			}
		}

		for cursor <= slots {
			starts[cursor] = end
			cursor++
		}
	}
}

// prefixRange returns the interval sharing the directory's cached prefix
// bits with key. Long-key searches cannot match outside this interval.
func (r *rootIndex) prefixRange(key uint64) (int32, int32) {
	bucket := int(key >> 48)
	width := r.radixBits[bucket]

	if width == 0 {
		return r.buckets[bucket], r.buckets[bucket+1]
	}

	mask := uint64(1)<<uint(width) - 1
	slot := int(key >> uint(48-width) & mask)
	base := int(r.radixBase[bucket]) + slot

	return r.radixStarts[base], r.radixStarts[base+1]
}

// link joins two different chains. The caller has checked that tail is free,
// head is free, and head is not the head of tail's own chain.
func (r *rootChains) link(tail, head int32, overlap int) {
	chainHead := r.other[tail]
	chainTail := r.other[head]

	r.succ[tail] = head
	r.hasPred[head] = true
	r.overlap[head] = uint8(overlap)

	r.other[chainHead] = chainTail
	r.other[chainTail] = chainHead
}

// overlapHead returns a free head having suffix as a prefix. Matching heads
// form one lexicographic interval, so at most the forbidden head needs to be
// skipped after finding the first available head.
//
// available == nil means every head is free and the index is read-only.
func (r *rootIndex) overlapHead(suffix string, forbidden int32, available []uint64) int32 {
	key := getPrefix64(suffix)
	if !r.mayHavePrefix(key) {
		return -1
	}

	low, high := r.prefixRange(key)

	for low < high {
		mid := low + (high-low)/2
		midKey := r.prefix[mid]

		if midKey < key || midKey == key && r.str(mid) < suffix {
			low = mid + 1
		} else {
			high = mid
		}
	}

	head := low

	if available != nil {
		head = availableRoot(available, head)
	}

	if head == forbidden {
		head++

		if available != nil {
			head = availableRoot(available, head)
		}
	}

	if int(head) >= len(r.roots) || r.prefix[head] != key ||
		!strings.HasPrefix(r.str(head), suffix) {
		return -1
	}

	return head
}

// longestOverlap searches suffixes in decreasing length. Proper overlaps are
// sufficient because all input-string containment has already been removed.
func (r *rootIndex) longestOverlap(tail int32, limit int, forbidden int32, available []uint64) (int, int32) {
	str := r.str(tail)

	for overlap := min(limit, len(str)-1); overlap > maxOverlapLevel; overlap-- {
		head := r.overlapHead(str[len(str)-overlap:], forbidden, available)
		if head != -1 {
			return overlap, head
		}
	}

	return 0, -1
}

// buildShortIndex separates two-byte roots from longer short roots.
//
// A three-to-seven-byte prefix leaves the low byte of its zero-padded key
// unused. Store the length there so a search probes one compact array rather
// than following root IDs into the full prefix and length arrays.
func (r *rootIndex) buildShortIndex() {
	var (
		hasTwo   bool
		numShort int
	)

	for _, ln := range r.length {
		switch {
		case ln == 2:
			hasTwo = true
		case ln >= 3 && ln < 8:
			numShort++
		}
	}

	if hasTwo {
		// Store root+1 so the zero-initialized table denotes absence.
		r.shortTwo = make([]int32, numBuckets)
	}

	if numShort != 0 {
		r.short = make([]int32, 0, numShort)
		r.shortKeys = make([]uint64, 0, numShort)
		r.shortBuckets = make([]int32, numBuckets+1)

		// Each two-byte bucket has an exact 256-bit set of third bytes.
		// This is a 2 MiB rejection filter with no hash collisions.
		r.shortThird = make([]uint64, numBuckets*4)
	}

	for root, ln := range r.length {
		if ln < 2 || ln >= 8 {
			continue
		}

		key := r.prefix[root]
		bucket := int(key >> 48)

		if ln == 2 {
			r.shortTwo[bucket] = int32(root) + 1

			continue
		}

		r.short = append(r.short, int32(root))
		r.shortKeys = append(r.shortKeys, key|uint64(ln))
		r.shortBuckets[bucket+1]++

		third := int(byte(key >> 40))
		r.shortThird[bucket*4+third/64] |= uint64(1) << uint(third%64)
	}

	for bucket := 0; bucket+1 < len(r.shortBuckets); bucket++ {
		r.shortBuckets[bucket+1] += r.shortBuckets[bucket]
	}
}

// shortContained returns a two-to-seven-byte root that prefixes suffix.
// One-byte roots are handled by the caller.
func (r *rootIndex) shortContained(suffix string, key uint64) int32 {
	if len(suffix) < 2 {
		return -1
	}

	bucket := int(key >> 48)

	if len(r.shortTwo) != 0 {
		root := r.shortTwo[bucket]

		if root != 0 {
			return root - 1
		}
	}

	if len(suffix) < 3 || len(r.shortKeys) == 0 {
		return -1
	}

	third := int(suffix[2])
	if r.shortThird[bucket*4+third/64]&(uint64(1)<<uint(third%64)) == 0 {
		return -1
	}

	start := r.shortBuckets[bucket]
	low := start
	high := r.shortBuckets[bucket+1]

	for low < high {
		mid := low + (high-low)/2

		// The low byte stores length, not part of the padded prefix.
		if r.shortKeys[mid]&^uint64(0xff) <= key {
			low = mid + 1
		} else {
			high = mid
		}
	}

	if low == start {
		return -1
	}

	word := r.shortKeys[low-1]
	ln := int(uint8(word))

	if ln > len(suffix) {
		return -1
	}

	mask := highMask(ln)
	if key&mask != word&mask {
		return -1
	}

	// Root IDs are only needed on a verified match, not at every probe.
	return r.short[low-1]
}

// compareSortWord compares records within one two-byte prefix bucket.
// The word caches bytes two through five; the bucket supplies bytes zero
// and one. Equal cached prefixes therefore settle the first six bytes.
func compareSortWord(a, b uint64, entries []string) int {
	if a>>32 != b>>32 {
		return cmp.Compare(a>>32, b>>32)
	}

	strA := entries[uint32(a)]
	strB := entries[uint32(b)]

	if len(strA) <= 6 || len(strB) <= 6 {
		// Equal zero-padded keys imply that the shorter string is a
		// prefix of the longer, including strings containing NUL.
		return cmp.Compare(len(strA), len(strB))
	}

	return cmp.Compare(strA[6:], strB[6:])
}

func newRootIndex(entries []string, representatives, roots []int32, prefix []uint64, length []uint8) *rootIndex {
	return newRootIndexWithShort(entries, representatives, roots, prefix, length, true)
}

func newRootIndexWithShort(entries []string, representatives, roots []int32, prefix []uint64, length []uint8, includeShort bool) *rootIndex {
	index := &rootIndex{
		entries:         entries,
		representatives: representatives,
		roots:           roots,
		prefix:          prefix,
		length:          length,
		buckets:         make([]int32, numBuckets+1),
		minLen:          MaxStringLen,
	}

	for i := range index.single {
		index.single[i] = -1
	}

	var (
		numShort int
		groups   int
		prev     uint64
	)

	for j, key := range prefix {
		ln := int(length[j])

		index.minLen = min(index.minLen, ln)
		index.buckets[int(key>>48)+1]++

		switch {
		case ln == 1:
			index.single[byte(key>>56)] = int32(j)
		case ln >= 2 && ln < 8:
			numShort++
		case ln >= 8:
			if groups == 0 || key != prev {
				groups++
				prev = key
			}
		}
	}

	for bucket := range numBuckets {
		index.buckets[bucket+1] += index.buckets[bucket]
	}

	index.buildRadix()

	if includeShort && numShort != 0 {
		index.buildShortIndex()
	}

	if groups != 0 {
		// Between 16 and 32 bits per distinct eight-byte prefix, apart
		// from the small minimum. All three bits occupy the same word
		// so a negative probe needs only one random memory access.
		words := 16

		for words < (groups+3)/4 {
			words *= 2
		}

		index.bloom = make([]uint64, words)

		var havePrev bool

		for j, key := range prefix {
			if length[j] < 8 || havePrev && key == prev {
				continue
			}

			hash := prefixHash(key)
			index.bloom[hash&uint64(words-1)] |= prefixBits(hash)

			prev = key
			havePrev = true
		}
	}

	return index
}

// removeInternalContainment parents roots found inside other roots and
// discovers longest-overlap candidates in the same suffix scan.
func removeInternalContainment(entries []string, representatives, roots []int32, prefix []uint64, length []uint8, parent []int32, parentOffset []uint8, numCPU int) []uint8 {
	candidates := make([]uint8, len(roots))

	if len(roots) <= 1 {
		return candidates
	}

	index := newRootIndex(entries, representatives, roots, prefix, length)

	// Even when every root is long, a useful overlap may be only nine
	// bytes. Containment's length bound alone would miss it.
	minSuffix := min(index.minLen, maxOverlapLevel+1)

	// Scan the frozen root set. Candidate lengths are upper bounds after
	// containment removes heads; linking will revalidate them.
	parallelFor(len(roots), numCPU, func(start, end int) {
		for host := start; host < end; host++ {
			str := index.str(int32(host))

			for pos := 1; pos+minSuffix <= len(str); pos++ {
				suffix := str[pos:]
				wantOverlap := candidates[host] == 0 && len(suffix) > maxOverlapLevel
				child, overlap := index.containedAndOverlap(suffix, int32(host), wantOverlap)

				if overlap {
					// Positions increase, so this is the longest proper overlap
					// for this tail in the frozen set.
					candidates[host] = uint8(len(suffix))
				}

				if child == -1 {
					continue
				}

				childUID := roots[child]

				// Only the winner writes the offset. It is not read until all
				// workers have completed.
				if atomic.CompareAndSwapInt32(&parent[childUID], -1, roots[host]) {
					parentOffset[childUID] = uint8(pos)
				}
			}
		}
	})

	return candidates
}

func newRootChains(length int, overlap []uint8) *rootChains {
	if overlap == nil {
		overlap = make([]uint8, length)
	}

	chains := &rootChains{
		succ:    make([]int32, length),
		other:   make([]int32, length),
		hasPred: make([]bool, length),
		overlap: overlap,
	}

	for j := range chains.succ {
		chains.succ[j] = -1
		chains.other[j] = int32(j)
	}

	return chains
}

func prefixHash(key uint64) uint64 {
	key ^= key >> 30
	key *= 0xbf58476d1ce4e5b9
	key ^= key >> 27
	key *= 0x94d049bb133111eb
	key ^= key >> 31

	return key
}

func prefixBits(hash uint64) uint64 {
	return uint64(1)<<(hash>>32&63) |
		uint64(1)<<(hash>>38&63) |
		uint64(1)<<(hash>>44&63)
}

// availableRoot is a successor set stored in the high halves of workspace.
// Low halves independently hold pending-tail links. len(workspace) is an
// implicit permanent sentinel, so no additional array element is needed.
func availableRoot(workspace []uint64, root int32) int32 {
	sentinel := int32(len(workspace))
	end := root

	for end != sentinel {
		next := int32(uint32(workspace[end] >> 32))
		if next == end {
			break
		}

		end = next
	}

	for root != end {
		word := workspace[root]
		next := int32(uint32(word >> 32))

		workspace[root] = uint64(uint32(end))<<32 | uint64(uint32(word))

		root = next
	}

	return end
}

func linkLongOverlaps(index *rootIndex, chains *rootChains, workspace []uint64) {
	var pending [MaxStringLen + 1]int32

	for i := range pending {
		pending[i] = -1
	}

	// Reuse the future short-overlap tail words for two independent uint32
	// arrays: pending links below, available-head links above. This avoids
	// allocating and collecting another eight bytes per root.
	//
	// overlap initially contains candidate bounds from containment. Build
	// every queue before clearing it for its final incoming-edge meaning.
	for tail := len(index.roots) - 1; tail >= 0; tail-- {
		overlap := int(chains.overlap[tail])
		chains.overlap[tail] = 0

		workspace[tail] = uint64(uint32(tail)) << 32

		if overlap > maxOverlapLevel {
			workspace[tail] |= uint64(uint32(pending[overlap]))
			pending[overlap] = int32(tail)
		}
	}

	for level := MaxStringLen - 1; level > maxOverlapLevel; level-- {
		for pending[level] != -1 {
			tail := pending[level]
			pending[level] = int32(uint32(workspace[tail]))

			overlap, head := index.longestOverlap(
				tail, level, chains.other[tail], workspace,
			)

			if overlap == 0 {
				continue
			}

			if overlap < level {
				workspace[tail] = workspace[tail]&highMask(4) |
					uint64(uint32(pending[overlap]))
				pending[overlap] = tail

				continue
			}

			chains.link(tail, head, overlap)

			next := availableRoot(workspace, head+1)

			workspace[head] = uint64(uint32(next))<<32 |
				uint64(uint32(workspace[head]))
		}
	}
}

func chainRoots(entries []string, representatives, roots []int32, prefix, suffix []uint64, length, candidates []uint8, numCPU int) *rootChains {
	chains := newRootChains(len(roots), candidates)

	// Long-overlap linking and short-overlap sorting reuse the same
	// words. The first short-overlap scatter overwrites the workspace.
	tails := make([]uint64, len(roots))

	index := newRootIndexWithShort(
		entries, representatives, roots, prefix, length, false,
	)

	linkLongOverlaps(index, chains, tails)

	buckets := index.buckets
	index = nil

	// Short overlaps still use the cache-friendly count/scatter/merge scheme.
	for level := maxOverlapLevel; level >= 1; level-- {
		shift := uint(64 - 8*level)
		mask := highMask(level)

		tailKey := func(word uint64, bucket int) uint64 {
			if level <= packedKeyLevel {
				return uint64(bucket)<<48 | (word>>32)<<16
			}

			return suffix[uint32(word)] << shift
		}

		tailOffsets := bucketPartition(
			len(roots), numCPU,
			func(j int) int {
				if chains.succ[j] != -1 || int(length[j]) < level {
					return -1
				}

				return int(suffix[j] << shift >> 48)
			},
			func(total int) {
				tails = tails[:total]
			},
			func(j int, pos int32) {
				key := suffix[j] << shift

				tails[pos] = key<<16&highMask(4) | uint64(uint32(j))
			},
		)

		switch {
		case level <= 2:
		case level <= packedKeyLevel:
			sortBucketsOrdered(tails, tailOffsets, numCPU)
		default:
			sortBuckets(tails, tailOffsets, numCPU, func(a, b uint64) int {
				if a>>32 != b>>32 {
					return cmp.Compare(a>>32, b>>32)
				}

				keyA := suffix[uint32(a)] << shift
				keyB := suffix[uint32(b)] << shift

				if keyA != keyB {
					return cmp.Compare(keyA, keyB)
				}

				return cmp.Compare(uint32(a), uint32(b))
			})
		}

		// Linking is sequential because different overlap keys can still
		// touch endpoints of the same chain. Check cycles before consuming
		// a head, rather than parking and later discarding a bad pairing.
		for bucket := range numBuckets {
			bucketTails := tails[tailOffsets[bucket]:tailOffsets[bucket+1]]
			if len(bucketTails) == 0 {
				continue
			}

			headEnd := bucket + 1

			if level == 1 {
				// Nonempty one-byte suffix buckets are multiples of 256.
				headEnd = bucket + 256
			}

			headPos := buckets[bucket]
			headHi := buckets[headEnd]

			for _, word := range bucketTails {
				key := tailKey(word, bucket)

				for headPos < headHi &&
					(chains.hasPred[headPos] ||
						int(length[headPos]) < level ||
						prefix[headPos]&mask < key) {
					headPos++
				}

				if headPos == headHi || prefix[headPos]&mask != key {
					continue
				}

				tail := int32(uint32(word))
				head := headPos

				if chains.other[tail] == head {
					head++

					for head < headHi &&
						(chains.hasPred[head] || int(length[head]) < level) {
						head++
					}

					if head == headHi || prefix[head]&mask != key {
						continue
					}
				}

				chains.link(tail, head, level)

				if head == headPos {
					headPos++
				}
			}
		}
	}

	chains.other = nil

	return chains
}

// planSubstringFreeBlob plans emission when no root is a substring of another.
//
// The accumulated blob ends with the previously emitted root. An overlap
// longer than that root would place the entire previous root inside the
// current root, contradicting substring-freeness. Consequently every overlap
// can be computed from the two adjacent roots independently.
//
// resolvedOffset temporarily stores predecessor root+1, with zero denoting
// the first emitted root. After overlap discovery, it receives final offsets.
func planSubstringFreeBlob(entries []string, representatives, roots []int32, length []uint8, chains *rootChains, resolvedOffset []uint64, numCPU int) (int, error) {
	previous := int32(-1)

	// Preserve the existing emission order, including chain boundaries.
	// Reuse the output-offset workspace instead of allocating predecessors.
	for start := range roots {
		if chains.hasPred[start] {
			continue
		}

		for curr := int32(start); curr != -1; curr = chains.succ[curr] {
			resolvedOffset[roots[curr]] = uint64(previous + 1)
			previous = curr
		}
	}

	// Each worker reads immutable strings and predecessor IDs and writes
	// only the overlap values belonging to its own root range.
	parallelFor(len(roots), numCPU, func(start, end int) {
		for curr := start; curr < end; curr++ {
			uid := roots[curr]
			predecessor := resolvedOffset[uid]

			if predecessor == 0 {
				chains.overlap[curr] = 0

				continue
			}

			previous := int32(predecessor - 1)
			prevStr := entries[representatives[roots[previous]]]
			str := entries[representatives[uid]]

			overlap := findStringOverlap(
				prevStr, str, int(chains.overlap[curr]),
			)

			chains.overlap[curr] = uint8(overlap)
		}
	})

	var total uint64

	maxInt := uint64(^uint(0) >> 1)

	// Only compact integer arrays are accessed in this serial pass.
	// No string headers, payload comparisons or history buffer are needed.
	for start := range roots {
		if chains.hasPred[start] {
			continue
		}

		for curr := int32(start); curr != -1; curr = chains.succ[curr] {
			overlap := uint64(chains.overlap[curr])
			next := total + uint64(length[curr]) - overlap

			if next > maxInt {
				return 0, ErrBlobTooLarge
			}

			resolvedOffset[roots[curr]] = total - overlap
			total = next
		}
	}

	return int(total), nil
}

// findStringOverlap returns the longest suffix of previous matching a prefix
// of str. known is a lower bound the caller has already verified.
func findStringOverlap(previous, str string, known int) int {
	maxOverlap := min(len(previous), len(str))
	if maxOverlap <= known {
		return known
	}

	first := str[0]
	last := previous[len(previous)-1]

	for overlap := maxOverlap; overlap > known; overlap-- {
		start := len(previous) - overlap

		if previous[start] == first && str[overlap-1] == last &&
			previous[start:] == str[:overlap] {
			return overlap
		}
	}

	return known
}
