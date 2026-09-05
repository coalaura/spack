package spack

import (
	"slices"
	"sync"
)

const (
	// Input indexes are below MaxInt32. After sorting, their unused high
	// bit records the first occurrence of each distinct string.
	sortWordUnique    uint64 = 1 << 31
	sortWordIndexMask uint32 = 1<<31 - 1

	// Small partitions use comparison sorting. This threshold bounds radix
	// setup overhead; it is a tuning parameter, not a correctness boundary.
	sortWordRadixMin = 4096
)

type sortWordTask struct {
	start int32
	end   int32
	shift uint
}

type wordSorter struct {
	words   []uint64
	compare func(a, b uint64) int

	jobs    chan sortWordTask
	pending sync.WaitGroup
}

// sortPackedBuckets sorts words whose high 32 bits are the primary key.
// The comparator resolves equal cached keys and may compare additional bytes.
// Each original bucket must have the same implicit leading key bytes.
//
// Large partitions use in-place byte radix partitioning. Workers can publish
// disjoint child partitions to a bounded queue; when full, they work locally.
func sortPackedBuckets(words []uint64, offsets []int32, numCPU int, compare func(a, b uint64) int) {
	if len(words) < sortWordRadixMin {
		sortBuckets(words, offsets, numCPU, compare)

		return
	}

	workers := max(1, numCPU)

	sorter := wordSorter{
		words:   words,
		compare: compare,
		jobs:    make(chan sortWordTask, workers),
	}

	var wg sync.WaitGroup

	for range workers {
		wg.Go(func() {
			for task := range sorter.jobs {
				sorter.sort(task)
				sorter.pending.Done()
			}
		})
	}

	for bucket := 0; bucket+1 < len(offsets); bucket++ {
		start := offsets[bucket]
		end := offsets[bucket+1]

		if end-start < 2 {
			continue
		}

		sorter.pending.Add(1)
		sorter.jobs <- sortWordTask{
			start: start,
			end:   end,
			shift: 56,
		}
	}

	sorter.pending.Wait()
	close(sorter.jobs)
	wg.Wait()
}

// schedule never blocks a worker on queue capacity. Otherwise all workers
// could wait to publish children with nobody left to consume the queue.
func (s *wordSorter) schedule(task sortWordTask) {
	if int(task.end-task.start) < sortWordRadixMin || task.shift < 32 {
		s.sort(task)

		return
	}

	s.pending.Add(1)

	select {
	case s.jobs <- task:
	default:
		s.pending.Done()
		s.sort(task)
	}
}

func (s *wordSorter) sort(task sortWordTask) {
	words := s.words[task.start:task.end]

	if len(words) < sortWordRadixMin || task.shift < 32 {
		slices.SortFunc(words, s.compare)

		return
	}

	var (
		counts    [256]int32
		starts    [257]int32
		cursor    [256]int32
		different uint64
	)

	first := words[0]

	for _, word := range words {
		counts[byte(word>>task.shift)]++
		different |= word ^ first
	}

	if different>>32 == 0 {
		// All cached keys are equal. More radix passes cannot help;
		// resolve lengths and remaining string bytes with the comparator.
		slices.SortFunc(words, s.compare)

		return
	}

	var occupied int

	starts[0] = task.start

	for bucket, count := range counts {
		starts[bucket+1] = starts[bucket] + count

		if count != 0 {
			occupied++
		}
	}

	if occupied == 1 {
		// Skip permutation when this byte does not distinguish any words.
		task.shift -= 8
		s.sort(task)

		return
	}

	copy(cursor[:], starts[:256])

	// American-flag partitioning: each cursor marks the next unfilled
	// position in its byte bucket. Swaps place one word into its final
	// bucket without allocating an output array.
	for bucket := range counts {
		for cursor[bucket] < starts[bucket+1] {
			pos := cursor[bucket]
			target := int(byte(s.words[pos] >> task.shift))

			if target == bucket {
				cursor[bucket]++

				continue
			}

			next := cursor[target]

			s.words[pos], s.words[next] = s.words[next], s.words[pos]
			cursor[target]++
		}
	}

	for bucket, count := range counts {
		if count < 2 {
			continue
		}

		s.schedule(sortWordTask{
			start: starts[bucket],
			end:   starts[bucket+1],
			shift: task.shift - 8,
		})
	}
}

// markUniqueWords marks unique-string boundaries in sorted words and returns
// the chunk size and exclusive per-chunk unique counts.
//
// Boundary comparisons happen before workers modify any words. Within each
// chunk, the previous unmodified word is kept locally, so no worker reads a
// neighboring worker's writes. No atomic operations or per-input bitmap are
// needed.
func markUniqueWords(words []uint64, entries []string, numCPU int) (int, []int32) {
	length := len(words)

	if length == 0 {
		return 1, []int32{0}
	}

	chunkSize := 1 + (length-1)/numCPU
	numChunks := 1 + (length-1)/chunkSize

	chunkBase := make([]int32, numChunks+1)
	boundaries := make([]bool, numChunks)

	different := func(previous, current uint64) bool {
		if previous>>32 != current>>32 {
			return true
		}

		// The cached bytes omit the original two-byte bucket. Whole-string
		// equality is required when comparing across bucket boundaries.
		return entries[uint32(previous)] != entries[uint32(current)]
	}

	boundaries[0] = true

	for chunk := 1; chunk < numChunks; chunk++ {
		start := chunk * chunkSize

		boundaries[chunk] = different(words[start-1], words[start])
	}

	parallelChunks(length, chunkSize, func(chunk, start, end int) {
		var (
			count    int32
			previous uint64
		)

		for i := start; i < end; i++ {
			current := words[i]
			unique := boundaries[chunk]

			if i != start {
				unique = different(previous, current)
			}

			if unique {
				words[i] = current | sortWordUnique
				count++
			}

			previous = current
		}

		chunkBase[chunk+1] = count
	})

	for chunk := range numChunks {
		chunkBase[chunk+1] += chunkBase[chunk]
	}

	return chunkSize, chunkBase
}
