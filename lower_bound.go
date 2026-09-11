package spack

import (
	"math"
	"math/bits"
	"runtime"
	"slices"
	"strings"
	"time"
)

// BlobSizeBound describes a certified lower bound for the one-blob shortest
// superstring problem after duplicate and contained roots have been removed.
// The overlap histograms use overlap lengths as indexes.
type BlobSizeBound struct {
	RootCount                   int
	RootBytes                   uint64
	LongestRootBytes            uint64
	OutgoingOverlapUpperBound   uint64
	IncomingOverlapUpperBound   uint64
	OverlapUpperBound           uint64
	LowerBoundBytes             uint64
	CurrentBlobBytes            uint64
	RootsWithoutOutgoingOverlap uint64
	RootsWithoutIncomingOverlap uint64
	OutgoingMaximumDistribution [MaxStringLen + 1]uint64
	IncomingMaximumDistribution [MaxStringLen + 1]uint64
	ComputationTime             time.Duration
}

type reverseOverlapIndex struct {
	index     *rootIndex
	positions []int32
}

func (r *reverseOverlapIndex) longestOverlap(tail int32) int {
	str := r.index.str(tail)

	for overlap := len(str) - 1; overlap > 0; overlap-- {
		head := r.overlapHead(str, overlap, tail)
		if head != -1 {
			return overlap
		}
	}

	return 0
}

func (r *reverseOverlapIndex) overlapHead(str string, overlap int, forbidden int32) int32 {
	key := reversedPrefixKey(str, overlap)

	var (
		low  int32
		high int32
	)

	if overlap >= 8 {
		if !r.index.mayHavePrefix(key) {
			return -1
		}

		low, high = r.index.prefixRange(key)
	} else {
		low, high = shortPrefixRange(r.index.buckets, key, overlap)
	}

	for low < high {
		mid := low + (high-low)/2
		candidate := r.index.str(mid)

		comparison := compareReversedToPrefix(candidate, str, overlap)

		if comparison < 0 {
			low = mid + 1
		} else {
			high = mid
		}
	}

	head := low
	if head == forbidden {
		head++
	}

	if int(head) >= len(r.positions) {
		return -1
	}

	candidate := r.index.str(head)
	if !strings.HasSuffix(candidate, str[:overlap]) {
		return -1
	}

	return head
}

func calculateBlobSizeBound(entries []string, representatives, roots []int32, prefix []uint64, length []uint8) (BlobSizeBound, error) {
	startTime := time.Now()

	bound := BlobSizeBound{
		RootCount: len(roots),
	}

	for _, rootLength := range length {
		value := uint64(rootLength)

		if bound.RootBytes > math.MaxUint64-value {
			return BlobSizeBound{}, ErrBlobTooLarge
		}

		bound.RootBytes += value
		bound.LongestRootBytes = max(bound.LongestRootBytes, value)
	}

	if len(roots) <= 1 {
		bound.LowerBoundBytes = bound.LongestRootBytes
		bound.RootsWithoutOutgoingOverlap = uint64(len(roots))
		bound.RootsWithoutIncomingOverlap = uint64(len(roots))
		bound.OutgoingMaximumDistribution[0] = uint64(len(roots))
		bound.IncomingMaximumDistribution[0] = uint64(len(roots))
		bound.ComputationTime = time.Since(startTime)

		return bound, nil
	}

	numCPU := runtime.GOMAXPROCS(0)

	forwardIndex := newRootIndexWithShort(entries, representatives, roots, prefix, length, false)
	outgoingMaximum := make([]uint8, len(roots))

	parallelFor(len(roots), numCPU, func(start, end int) {
		for tail := start; tail < end; tail++ {
			outgoingMaximum[tail] = uint8(longestCompleteOverlap(forwardIndex, int32(tail)))
		}
	})

	reverseIndex := newReverseOverlapIndex(
		entries, representatives, roots, length,
	)
	incomingMaximum := make([]uint8, len(roots))

	parallelFor(len(roots), numCPU, func(start, end int) {
		for tail := start; tail < end; tail++ {
			position := reverseIndex.positions[tail]

			incomingMaximum[position] = uint8(reverseIndex.longestOverlap(int32(tail)))
		}
	})

	outgoingSum, outgoingMinimum, err := summarizeMaximumOverlaps(outgoingMaximum, &bound.OutgoingMaximumDistribution)
	if err != nil {
		return BlobSizeBound{}, err
	}

	incomingSum, incomingMinimum, err := summarizeMaximumOverlaps(incomingMaximum, &bound.IncomingMaximumDistribution)
	if err != nil {
		return BlobSizeBound{}, err
	}

	bound.RootsWithoutOutgoingOverlap = bound.OutgoingMaximumDistribution[0]
	bound.RootsWithoutIncomingOverlap = bound.IncomingMaximumDistribution[0]

	bound.OutgoingOverlapUpperBound = outgoingSum - uint64(outgoingMinimum)
	bound.IncomingOverlapUpperBound = incomingSum - uint64(incomingMinimum)

	bound.OverlapUpperBound = min(bound.OutgoingOverlapUpperBound, bound.IncomingOverlapUpperBound)
	bound.LowerBoundBytes = max(bound.LongestRootBytes, bound.RootBytes-bound.OverlapUpperBound)

	bound.ComputationTime = time.Since(startTime)

	return bound, nil
}

func newReverseOverlapIndex(entries []string, representatives, roots []int32, length []uint8) *reverseOverlapIndex {
	positions := make([]int32, len(roots))

	for position := range positions {
		positions[position] = int32(position)
	}

	slices.SortFunc(positions, func(first, second int32) int {
		firstString := entries[representatives[roots[first]]]
		secondString := entries[representatives[roots[second]]]

		return compareReversed(firstString, secondString)
	})

	reverseRoots := make([]int32, len(roots))
	reversePrefix := make([]uint64, len(roots))
	reverseLength := make([]uint8, len(roots))

	for reversePosition, position := range positions {
		reverseRoots[reversePosition] = roots[position]

		reversePrefix[reversePosition] = bits.ReverseBytes64(getSuffixWindow64(entries[representatives[roots[position]]]))

		reverseLength[reversePosition] = length[position]
	}

	index := newRootIndexWithShort(entries, representatives, reverseRoots, reversePrefix, reverseLength, false)

	return &reverseOverlapIndex{
		index:     index,
		positions: positions,
	}
}

func longestCompleteOverlap(index *rootIndex, tail int32) int {
	str := index.str(tail)
	limit := len(str) - 1

	overlap, _ := index.longestOverlap(tail, limit, tail, nil)
	if overlap != 0 {
		return overlap
	}

	for overlap := min(limit, maxOverlapLevel); overlap > 0; overlap-- {
		if shortOverlapHead(index, str[len(str)-overlap:], tail) != -1 {
			return overlap
		}
	}

	return 0
}

func shortOverlapHead(index *rootIndex, suffix string, forbidden int32) int32 {
	key := getPrefix64(suffix)
	low, high := shortPrefixRange(index.buckets, key, len(suffix))

	for low < high {
		mid := low + (high-low)/2

		if index.str(mid) < suffix {
			low = mid + 1
		} else {
			high = mid
		}
	}

	head := low
	if head == forbidden {
		head++
	}

	if int(head) >= len(index.roots) || !strings.HasPrefix(index.str(head), suffix) {
		return -1
	}

	return head
}

func shortPrefixRange(buckets []int32, key uint64, length int) (int32, int32) {
	bucket := int(key >> 48)

	if length == 1 {
		bucket &= 0xff00

		return buckets[bucket], buckets[bucket+0x100]
	}

	return buckets[bucket], buckets[bucket+1]
}

func reversedPrefixKey(str string, length int) uint64 {
	var key uint64

	start := max(0, length-8)

	for position := length - 1; position >= start; position-- {
		key = key<<8 | uint64(str[position])
	}

	return key << (64 - 8*uint(length-start))
}

func compareReversedToPrefix(candidate, str string, length int) int {
	compared := min(len(candidate), length)

	for offset := range compared {
		candidateByte := candidate[len(candidate)-1-offset]
		queryByte := str[length-1-offset]

		if candidateByte < queryByte {
			return -1
		}

		if candidateByte > queryByte {
			return 1
		}
	}

	if len(candidate) < length {
		return -1
	}

	if len(candidate) > length {
		return 1
	}

	return 0
}

func summarizeMaximumOverlaps(maximum []uint8, distribution *[MaxStringLen + 1]uint64) (uint64, uint8, error) {
	minimum := uint8(MaxStringLen)

	var total uint64

	for _, overlap := range maximum {
		value := uint64(overlap)

		if total > math.MaxUint64-value {
			return 0, 0, ErrBlobTooLarge
		}

		total += value

		minimum = min(minimum, overlap)

		distribution[overlap]++
	}

	return total, minimum, nil
}
