package spack

import "math"

// planBlob calculates final offsets and the exact allocation size. A small
// sliding buffer retains the last 255 bytes needed for overlap decisions
// without constructing a provisional multi-gigabyte blob.
func planBlob(entries []string, representatives, roots []int32, chains *rootChains, resolvedOffset []uint32) (int, error) {
	var (
		tail    [4096]byte
		tailLen int
		total   uint64
	)

	maxInt := uint64(^uint(0) >> 1)

	for start := range roots {
		if chains.hasPred[start] {
			continue
		}

		for curr := int32(start); curr != -1; curr = chains.succ[curr] {
			uid := roots[curr]
			str := entries[representatives[uid]]

			overlap := findOverlap(tail[:tailLen], str, int(chains.overlap[curr]))
			appended := str[overlap:]
			next := total + uint64(len(appended))

			if next > math.MaxUint32 || next > maxInt {
				return 0, ErrBlobTooLarge
			}

			resolvedOffset[uid] = uint32(total - uint64(overlap))
			chains.overlap[curr] = uint8(overlap)

			// Append into a small sliding buffer instead of moving the
			// last 255 bytes for every root. Only compact when full;
			// findOverlap never examines more than MaxStringLen bytes.
			if len(appended) > len(tail)-tailLen {
				keep := min(tailLen, MaxStringLen)

				copy(tail[:keep], tail[tailLen-keep:tailLen])
				tailLen = keep
			}

			tailLen += copy(tail[tailLen:], appended)
			total = next
		}
	}

	return int(total), nil
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
