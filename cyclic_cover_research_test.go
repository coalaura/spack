package spack

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	cyclicCoverCorpusMagic = "SPACKC01"
	cyclicCoverShardCount  = 256
	cyclicCoverEmptyKey    = math.MaxUint64
)

type cyclicCoverResearchResult struct {
	RootCount              int
	RootBytes              uint64
	EHOGNodeCount          int
	ProperOverlapNodeCount int
	OverlapSavings         uint64
	NormBytes              uint64
	ComponentCount         int
	LinearizationCost      uint64
	CandidateBytes         uint64
	EulerOverlapSavings    uint64
	EulerCutCost           uint64
	EulerCandidateBytes    uint64
	ComponentJoinCount     int
	ComponentJoinSavings   uint64
	BaselineBytes          uint64
	JoinedCandidateBytes   uint64
	CandidateApplied       bool
	ComputationTime        time.Duration
}

type cyclicCoverNode struct {
	key          uint64
	root         int32
	prefixParent int32
	suffixParent int32
	length       uint8
	isRoot       bool
}

type cyclicCoverNodeSetShard struct {
	sync.Mutex

	keys map[uint64]struct{}
}

type cyclicCoverDisjointSet struct {
	parent  []int32
	minimum []uint8
}

type cyclicCoverMemoryMonitor struct {
	stop chan struct{}
	done chan struct{}

	peakAlloc uint64
}

type cyclicCoverOracleCase struct {
	name  string
	roots []string
}

func (m *cyclicCoverMemoryMonitor) run() {
	defer close(m.done)

	var stats runtime.MemStats

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		runtime.ReadMemStats(&stats)
		m.peakAlloc = max(m.peakAlloc, stats.Alloc)

		select {
		case <-m.stop:
			return
		case <-ticker.C:
		}
	}
}

func (m *cyclicCoverMemoryMonitor) stopAndWait() uint64 {
	close(m.stop)
	<-m.done

	return m.peakAlloc
}

func (d *cyclicCoverDisjointSet) find(node int32) int32 {
	root := node

	for d.parent[root] != root {
		root = d.parent[root]
	}

	for node != root {
		parent := d.parent[node]

		d.parent[node] = root
		node = parent
	}

	return root
}

func (d *cyclicCoverDisjointSet) union(first, second int32) {
	first = d.find(first)
	second = d.find(second)

	if first == second {
		return
	}

	d.parent[second] = first
	d.minimum[first] = min(d.minimum[first], d.minimum[second])
}

func TestCyclicCoverEvaluatorAgainstIndependentOracles(t *testing.T) {
	t.Parallel()

	tests := []cyclicCoverOracleCase{
		{
			name: "empty",
		},
		{
			name:  "single periodic root",
			roots: []string{"abab"},
		},
		{
			name:  "disconnected periodic cycles",
			roots: []string{"abab", "baba", "cdcd", "dcdc"},
		},
		{
			name:  "competing overlaps",
			roots: []string{"aaab", "aaba", "abaa", "baaa", "bbba"},
		},
		{
			name:  "arbitrary bytes",
			roots: []string{"\x00\xffa", "a\x00\xff", "\x80\x00b", "\xffa\x00"},
		},
		{
			name:  "more than refinement limit",
			roots: overlappingRoots(12, 48),
		},
		{
			name: "maximum supported length",
			roots: []string{
				"0" + strings.Repeat("ab", 127),
				strings.Repeat("ab", 127) + "1",
				"2" + strings.Repeat("cd", 127),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			roots := oracleSubstringFreeRoots(test.roots)

			result := evaluateCyclicCoverStrings(t, roots)
			wantSavings := exactCyclicCoverOverlapSavings(roots)

			if result.OverlapSavings != uint64(wantSavings) {
				t.Fatalf("overlap savings %d, want %d", result.OverlapSavings, wantSavings)
			}

			optimal := exactOracleSuperstringLength(roots)
			if result.NormBytes > uint64(optimal) {
				t.Fatalf("cyclic-cover bound %d exceeds exact linear optimum %d", result.NormBytes, optimal)
			}

			if result.CandidateBytes < uint64(optimal) {
				t.Fatalf("MGreedyMin candidate %d is shorter than exact optimum %d", result.CandidateBytes, optimal)
			}
		})
	}
}

func TestCyclicCoverEvaluatorRandomizedAgainstIndependentOracles(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(0xc7c1, 0xe40f))
	alphabet := []byte{0, 1, 'a', 'b', 0x80, 0xff}

	for iteration := range 300 {
		input := make([]string, rng.IntN(9))

		for index := range input {
			value := make([]byte, rng.IntN(9))

			for position := range value {
				value[position] = alphabet[rng.IntN(len(alphabet))]
			}

			input[index] = string(value)
		}

		roots := oracleSubstringFreeRoots(input)

		result := evaluateCyclicCoverStrings(t, roots)
		wantSavings := exactCyclicCoverOverlapSavings(roots)

		if result.OverlapSavings != uint64(wantSavings) {
			t.Fatalf("iteration %d overlap savings %d, want %d for %q", iteration, result.OverlapSavings, wantSavings, roots)
		}

		optimal := exactOracleSuperstringLength(roots)
		if result.NormBytes > uint64(optimal) {
			t.Fatalf("iteration %d cyclic-cover bound %d exceeds exact optimum %d for %q", iteration, result.NormBytes, optimal, roots)
		}
	}
}

func TestCyclicCoverCandidateRoundTrip(t *testing.T) {
	t.Parallel()

	input := []string{
		"cdcabdab",
		"bacdacdb",
		"bbbcabbd",
		"dbcbbccc",
		"bcdbbdcb",
		"cbababad",
		"dbddcaac",
		"daaaddbb",
		"ccbdbdbd",
		"ddcccbad",
		"bcacacbb",
		"bcaadcab",
	}

	input = append(input,
		input[2],
		input[5][2:6],
		"",
	)

	var result cyclicCoverResearchResult

	hook := func(entries []string, representatives, roots []int32, prefix, _ []uint64, length []uint8, chains *rootChains) error {
		var err error

		result, err = evaluateCyclicCover(entries, representatives, roots, prefix, length, chains)

		return err
	}

	packed, err := NewStringMap(input).pack[Pointer32](nil, hook)
	if err != nil {
		t.Fatalf("pack cyclic-cover candidate: %v", err)
	}

	if !result.CandidateApplied {
		t.Fatal("cyclic-cover candidate was not applied")
	}

	if result.JoinedCandidateBytes > result.EulerCandidateBytes {
		t.Fatalf("joined candidate %d exceeds Euler cycle linearization %d", result.JoinedCandidateBytes, result.EulerCandidateBytes)
	}

	if uint64(packed.Len()) != result.JoinedCandidateBytes {
		t.Fatalf("packed candidate length %d, want %d", packed.Len(), result.JoinedCandidateBytes)
	}

	for index, pointer := range packed.Pointers() {
		value, getErr := packed.GetStringUnsafe(pointer)
		if getErr != nil {
			t.Fatalf("resolve pointer %d: %v", index, getErr)
		}

		if value != input[index] {
			t.Fatalf("pointer %d resolved to %q, want %q", index, value, input[index])
		}
	}
}

func TestCyclicCoverResearchCorpus(t *testing.T) {
	corpusPath := os.Getenv("SPACK_RESEARCH_CORPUS_PATH")
	if corpusPath == "" {
		t.Skip("set SPACK_RESEARCH_CORPUS_PATH to a local .spc corpus file")
	}

	if filepath.Ext(corpusPath) != ".spc" {
		t.Fatalf("corpus file must use the .spc extension: %q", corpusPath)
	}

	file, err := os.Open(corpusPath)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}

	t.Cleanup(func() {
		err := file.Close()
		if err != nil {
			t.Errorf("close corpus: %v", err)
		}
	})

	entries, err := readCyclicCoverCorpus(file)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}

	t.Logf("corpus=%q inputs=%d GOMAXPROCS=%d %s/%s %s", corpusPath, len(entries), runtime.GOMAXPROCS(0), runtime.GOOS, runtime.GOARCH, runtime.Version())

	var (
		result     cyclicCoverResearchResult
		baseMemory runtime.MemStats
		monitor    *cyclicCoverMemoryMonitor
	)

	if os.Getenv("SPACK_RESEARCH_MEMORY") == "1" {
		runtime.GC()
		runtime.ReadMemStats(&baseMemory)

		monitor = &cyclicCoverMemoryMonitor{
			stop: make(chan struct{}),
			done: make(chan struct{}),
		}

		go monitor.run()
	}

	hook := func(entries []string, representatives, roots []int32, prefix, _ []uint64, length []uint8, chains *rootChains) error {
		var evaluateErr error

		var candidateChains *rootChains

		if os.Getenv("SPACK_RESEARCH_CONSTRUCT") == "1" {
			candidateChains = chains
		}

		result, evaluateErr = evaluateCyclicCover(entries, representatives, roots, prefix, length, candidateChains)

		return evaluateErr
	}

	startTime := time.Now()
	packed, err := NewStringMap(entries).pack[Pointer32](nil, hook)
	packTime := time.Since(startTime)

	var peakMemory uint64

	if monitor != nil {
		peakMemory = monitor.stopAndWait()
	}

	if err != nil {
		t.Fatalf("pack with cyclic-cover research hook: %v", err)
	}

	for index, pointer := range packed.Pointers() {
		value, getErr := packed.GetStringUnsafe(pointer)
		if getErr != nil {
			t.Fatalf("resolve pointer %d: %v", index, getErr)
		}

		if value != entries[index] {
			t.Fatalf("pointer %d resolved to %q, want %q", index, value, entries[index])
		}
	}

	packedBytes := uint64(packed.Len())
	if result.NormBytes > packedBytes {
		t.Fatalf("cyclic-cover norm %d exceeds packed blob %d", result.NormBytes, packedBytes)
	}

	gap := packedBytes - result.NormBytes
	percentage := float64(gap) / float64(packed.Len()) * 100

	t.Logf("roots=%d root_bytes=%d ehog_nodes=%d proper_overlap_nodes=%d", result.RootCount, result.RootBytes, result.EHOGNodeCount, result.ProperOverlapNodeCount)
	t.Logf("overlap_savings=%d norm=%d components=%d linearization_cost=%d candidate=%d", result.OverlapSavings, result.NormBytes, result.ComponentCount, result.LinearizationCost, result.CandidateBytes)

	if result.BaselineBytes != 0 {
		t.Logf("euler_overlap_savings=%d euler_cut=%d euler_candidate=%d", result.EulerOverlapSavings, result.EulerCutCost, result.EulerCandidateBytes)
		t.Logf("component_join_count=%d component_join_savings=%d", result.ComponentJoinCount, result.ComponentJoinSavings)
		t.Logf("baseline=%d joined_candidate=%d candidate_applied=%t", result.BaselineBytes, result.JoinedCandidateBytes, result.CandidateApplied)
	}

	t.Logf("current_blob=%d certified_gap=%d certified_gap_percent=%.6f", packed.Len(), gap, percentage)
	t.Logf("evaluator_time=%s full_pack_and_evaluator_time=%s", result.ComputationTime, packTime)

	if monitor != nil {
		t.Logf("memory_scope=full_pack_and_evaluator pre_call_heap=%d sampled_peak_heap=%d", baseMemory.Alloc, peakMemory)
	}
}

func evaluateCyclicCoverStrings(t *testing.T, roots []string) cyclicCoverResearchResult {
	t.Helper()

	slices.Sort(roots)

	rootIDs := make([]int32, len(roots))
	prefix := make([]uint64, len(roots))
	length := make([]uint8, len(roots))

	for index, root := range roots {
		rootIDs[index] = int32(index)
		prefix[index] = getPrefix64(root)
		length[index] = uint8(len(root))
	}

	result, err := evaluateCyclicCover(roots, rootIDs, rootIDs, prefix, length, nil)
	if err != nil {
		t.Fatalf("evaluate cyclic cover: %v", err)
	}

	return result
}

func evaluateCyclicCover(entries []string, representatives, roots []int32, prefix []uint64, length []uint8, candidateChains *rootChains) (cyclicCoverResearchResult, error) {
	startTime := time.Now()

	result := cyclicCoverResearchResult{
		RootCount: len(roots),
	}

	if len(roots) == 0 {
		result.ComputationTime = time.Since(startTime)

		return result, nil
	}

	if len(roots) == 1 && length[0] == 0 {
		result.EHOGNodeCount = 1
		result.ComponentCount = 1
		result.ComputationTime = time.Since(startTime)

		return result, nil
	}

	index := newRootIndexWithShort(entries, representatives, roots, prefix, length, false)

	shards := make([]cyclicCoverNodeSetShard, cyclicCoverShardCount)

	parallelFor(len(roots), runtime.GOMAXPROCS(0), func(start, end int) {
		for tail := start; tail < end; tail++ {
			value := index.str(int32(tail))

			for offset := 1; offset < len(value); offset++ {
				suffix := value[offset:]

				head := cyclicCoverPrefixRoot(index, suffix)
				if head == -1 {
					continue
				}

				key := cyclicCoverNodeKey(head, len(suffix))
				shard := &shards[byte(key>>8)]

				shard.Lock()

				if shard.keys == nil {
					shard.keys = make(map[uint64]struct{})
				}

				shard.keys[key] = struct{}{}
				shard.Unlock()
			}
		}
	})

	properOverlapCount := 0

	for shard := range shards {
		properOverlapCount += len(shards[shard].keys)
	}

	nodes := make([]cyclicCoverNode, 0, len(roots)+properOverlapCount+1)

	nodes = append(nodes, cyclicCoverNode{
		key:          cyclicCoverEmptyKey,
		prefixParent: -1,
		suffixParent: -1,
	})

	for shard := range shards {
		for key := range shards[shard].keys {
			nodes = append(nodes, cyclicCoverNode{
				key:          key,
				root:         int32(key >> 8),
				prefixParent: -1,
				suffixParent: -1,
				length:       uint8(key),
			})
		}
	}

	for root, rootLength := range length {
		value := uint64(rootLength)
		if result.RootBytes > math.MaxUint64-value {
			return cyclicCoverResearchResult{}, ErrBlobTooLarge
		}

		result.RootBytes += value

		nodes = append(nodes, cyclicCoverNode{
			key:          cyclicCoverNodeKey(int32(root), int(rootLength)),
			root:         int32(root),
			prefixParent: -1,
			suffixParent: -1,
			length:       rootLength,
			isRoot:       true,
		})
	}

	slices.SortFunc(nodes, func(first, second cyclicCoverNode) int {
		if first.length != second.length {
			return int(second.length) - int(first.length)
		}

		if first.isRoot != second.isRoot {
			if first.isRoot {
				return -1
			}

			return 1
		}

		return cmp.Compare(first.key, second.key)
	})

	nodeByKey := make(map[uint64]int32, len(nodes))

	for node := range nodes {
		nodeByKey[nodes[node].key] = int32(node)
	}

	for node := range nodes {
		if nodes[node].length == 0 {
			continue
		}

		value := index.str(nodes[node].root)[:nodes[node].length]

		nodes[node].prefixParent = cyclicCoverPrefixParent(index, nodeByKey, value)
		nodes[node].suffixParent = cyclicCoverSuffixParent(index, nodeByKey, value)
	}

	suffixFlow := make([]uint64, len(nodes))
	prefixFlow := make([]uint64, len(nodes))

	disjointSet := newCyclicCoverDisjointSet(nodes)

	for node := range nodes {
		if nodes[node].isRoot {
			suffixFlow[node]++
			prefixFlow[node]++
		} else {
			uses := min(suffixFlow[node], prefixFlow[node])
			depth := uint64(nodes[node].length)

			if uses != 0 && depth > math.MaxUint64/uses {
				return cyclicCoverResearchResult{}, ErrBlobTooLarge
			}

			saving := uses * depth
			if result.OverlapSavings > math.MaxUint64-saving {
				return cyclicCoverResearchResult{}, ErrBlobTooLarge
			}

			result.OverlapSavings += saving

			suffixFlow[node] -= uses
			prefixFlow[node] -= uses
		}

		if suffixFlow[node] != 0 {
			parent := nodes[node].suffixParent
			if parent == -1 {
				return cyclicCoverResearchResult{}, fmt.Errorf("positive suffix flow at root node %d", node)
			}

			suffixFlow[parent] += suffixFlow[node]
			disjointSet.union(int32(node), parent)
		}

		if prefixFlow[node] != 0 {
			parent := nodes[node].prefixParent
			if parent == -1 {
				return cyclicCoverResearchResult{}, fmt.Errorf("positive prefix flow at root node %d", node)
			}

			prefixFlow[parent] += prefixFlow[node]
			disjointSet.union(int32(node), parent)
		}
	}

	components := make(map[int32]struct{})

	for node := range nodes {
		if !nodes[node].isRoot {
			continue
		}

		component := disjointSet.find(int32(node))
		components[component] = struct{}{}
	}

	for component := range components {
		result.LinearizationCost += uint64(disjointSet.minimum[component])
	}

	result.EHOGNodeCount = len(nodes)
	result.ProperOverlapNodeCount = properOverlapCount
	result.NormBytes = result.RootBytes - result.OverlapSavings
	result.ComponentCount = len(components)
	result.CandidateBytes = result.NormBytes + result.LinearizationCost

	if candidateChains != nil {
		result.BaselineBytes = cyclicCoverChainBlobLength(index, candidateChains)

		constructedChains := &rootChains{
			succ:    make([]int32, len(roots)),
			hasPred: make([]bool, len(roots)),
			overlap: make([]uint8, len(roots)),
		}

		constructedSavings, constructedCut, constructErr := constructCyclicCoverCandidate(index, nodes, suffixFlow, prefixFlow, constructedChains)
		if constructErr != nil {
			return cyclicCoverResearchResult{}, constructErr
		}

		if constructedSavings < result.OverlapSavings {
			return cyclicCoverResearchResult{}, fmt.Errorf("Euler construction has overlap savings %d below certified cycle-cover savings %d", constructedSavings, result.OverlapSavings)
		}

		result.EulerOverlapSavings = constructedSavings
		result.EulerCutCost = constructedCut
		result.EulerCandidateBytes = result.RootBytes - constructedSavings + constructedCut

		if result.EulerCandidateBytes < result.NormBytes {
			return cyclicCoverResearchResult{}, fmt.Errorf("Euler candidate %d is below certified norm %d", result.EulerCandidateBytes, result.NormBytes)
		}

		storedCandidateBytes := cyclicCoverStoredChainLength(index, constructedChains)
		if storedCandidateBytes != result.EulerCandidateBytes {
			return cyclicCoverResearchResult{}, fmt.Errorf("stored cycle chains have length %d, want %d", storedCandidateBytes, result.EulerCandidateBytes)
		}

		result.ComponentJoinCount, result.ComponentJoinSavings = optimizeCyclicCoverComponentJoins(index, constructedChains)
		result.JoinedCandidateBytes = cyclicCoverChainBlobLength(index, constructedChains)

		if result.JoinedCandidateBytes < result.BaselineBytes {
			result.CandidateApplied = true
			*candidateChains = *constructedChains
		}
	}

	result.ComputationTime = time.Since(startTime)

	return result, nil
}

func optimizeCyclicCoverComponentJoins(index *rootIndex, chains *rootChains) (int, uint64) {
	starts := make([]int32, 0)
	ends := make([]int32, 0)

	for start := range chains.succ {
		if chains.hasPred[start] {
			continue
		}

		end := int32(start)

		for chains.succ[end] != -1 {
			end = chains.succ[end]
		}

		starts = append(starts, int32(start))
		ends = append(ends, end)
	}

	if len(starts) < 2 || len(starts) >= 1<<24 {
		return 0, 0
	}

	edges := make([]uint64, 0, min(len(starts)*8, 1<<20))

	for tail, tailRoot := range ends {
		tailString := index.str(tailRoot)

		for head, headRoot := range starts {
			if tail == head {
				continue
			}

			overlap := findStringOverlap(tailString, index.str(headRoot), 0)
			if overlap == 0 {
				continue
			}

			edge := uint64(overlap)<<48 | uint64(tail)<<24 | uint64(head)
			edges = append(edges, edge)
		}
	}

	slices.Sort(edges)

	disjointSet := &cyclicCoverDisjointSet{
		parent:  make([]int32, len(starts)),
		minimum: make([]uint8, len(starts)),
	}

	successorSet := make([]bool, len(starts))
	predecessorSet := make([]bool, len(starts))

	for component := range starts {
		disjointSet.parent[component] = int32(component)
	}

	var (
		joinCount   int
		joinSavings uint64
	)

	for _, edge := range slices.Backward(edges) {

		tail := int32(edge >> 24 & 0xffffff)
		head := int32(edge & 0xffffff)

		if successorSet[tail] || predecessorSet[head] || disjointSet.find(tail) == disjointSet.find(head) {
			continue
		}

		overlap := uint8(edge >> 48)

		chains.succ[ends[tail]] = starts[head]
		chains.hasPred[starts[head]] = true
		chains.overlap[starts[head]] = overlap

		successorSet[tail] = true
		predecessorSet[head] = true

		disjointSet.union(tail, head)

		joinCount++
		joinSavings += uint64(overlap)
	}

	return joinCount, joinSavings
}

func constructCyclicCoverCandidate(index *rootIndex, nodes []cyclicCoverNode, suffixEdges, prefixEdges []uint64, chains *rootChains) (uint64, uint64, error) {
	firstPrefixChild := make([]int32, len(nodes))
	nextPrefixChild := make([]int32, len(nodes))

	for node := range nodes {
		firstPrefixChild[node] = -1
		nextPrefixChild[node] = -1
	}

	for child := range nodes {
		if prefixEdges[child] == 0 {
			continue
		}

		parent := nodes[child].prefixParent
		nextPrefixChild[child] = firstPrefixChild[parent]
		firstPrefixChild[parent] = int32(child)
	}

	for root := range chains.succ {
		chains.succ[root] = -1
	}

	stack := make([]int32, 0, len(chains.succ))
	cycleRoots := make([]int32, 0, len(chains.succ))
	rootOccurrences := make([]uint8, len(chains.succ))

	var (
		overlapSavings uint64
		cutCost        uint64
	)

	for start := range nodes {
		if !cyclicCoverHasOutgoingEdge(start, firstPrefixChild, suffixEdges) {
			continue
		}

		cycleStart := len(cycleRoots)
		stack = append(stack, int32(start))

		for len(stack) != 0 {
			node := stack[len(stack)-1]
			child := firstPrefixChild[node]

			if child != -1 {
				prefixEdges[child]--

				if prefixEdges[child] == 0 {
					firstPrefixChild[node] = nextPrefixChild[child]
				}

				stack = append(stack, child)

				continue
			}

			if suffixEdges[node] != 0 {
				suffixEdges[node]--
				stack = append(stack, nodes[node].suffixParent)

				continue
			}

			stack = stack[:len(stack)-1]

			if nodes[node].isRoot {
				cycleRoots = append(cycleRoots, nodes[node].root)
			}
		}

		cycle := cycleRoots[cycleStart:]

		slices.Reverse(cycle)

		if len(cycle) > 1 && cycle[0] == cycle[len(cycle)-1] {
			cycle = cycle[:len(cycle)-1]
			cycleRoots = cycleRoots[:len(cycleRoots)-1]
		}

		if len(cycle) == 0 {
			continue
		}

		for _, root := range cycle {
			rootOccurrences[root]++
		}

		cut := 0
		overlaps := make([]uint8, len(cycle))

		for tail := range cycle {
			head := (tail + 1) % len(cycle)
			overlap := cyclicCoverRootOverlap(index, cycle[tail], cycle[head])

			overlaps[tail] = uint8(overlap)
			overlapSavings += uint64(overlap)

			if overlaps[tail] < overlaps[cut] {
				cut = tail
			}
		}

		cutCost += uint64(overlaps[cut])

		for position := 0; position < len(cycle)-1; position++ {
			tailPosition := (cut + 1 + position) % len(cycle)
			headPosition := (tailPosition + 1) % len(cycle)

			tail := cycle[tailPosition]
			head := cycle[headPosition]

			chains.succ[tail] = head
			chains.hasPred[head] = true
			chains.overlap[head] = overlaps[tailPosition]
		}
	}

	for root, occurrences := range rootOccurrences {
		if occurrences != 1 {
			return 0, 0, fmt.Errorf("Euler traversal emitted root %d %d times", root, occurrences)
		}
	}

	return overlapSavings, cutCost, nil
}

func cyclicCoverHasOutgoingEdge(node int, firstPrefixChild []int32, suffixEdges []uint64) bool {
	return firstPrefixChild[node] != -1 || suffixEdges[node] != 0
}

func cyclicCoverRootOverlap(index *rootIndex, tail, head int32) int {
	if tail == head {
		value := index.str(tail)

		return oracleProperStringOverlap(value, value, true)
	}

	return findStringOverlap(index.str(tail), index.str(head), 0)
}

func cyclicCoverChainBlobLength(index *rootIndex, chains *rootChains) uint64 {
	var (
		previous int32 = -1
		total    uint64
	)

	for start := range chains.succ {
		if chains.hasPred[start] {
			continue
		}

		for current := int32(start); current != -1; current = chains.succ[current] {
			value := index.str(current)
			overlap := 0

			if previous != -1 {
				overlap = findStringOverlap(index.str(previous), value, int(chains.overlap[current]))
			}

			total += uint64(len(value) - overlap)
			previous = current
		}
	}

	return total
}

func cyclicCoverStoredChainLength(index *rootIndex, chains *rootChains) uint64 {
	var total uint64

	for root := range chains.succ {
		total += uint64(len(index.str(int32(root))))

		if chains.hasPred[root] {
			total -= uint64(chains.overlap[root])
		}
	}

	return total
}

func newCyclicCoverDisjointSet(nodes []cyclicCoverNode) *cyclicCoverDisjointSet {
	disjointSet := &cyclicCoverDisjointSet{
		parent:  make([]int32, len(nodes)),
		minimum: make([]uint8, len(nodes)),
	}

	for node := range nodes {
		disjointSet.parent[node] = int32(node)
		disjointSet.minimum[node] = nodes[node].length
	}

	return disjointSet
}

func cyclicCoverPrefixParent(index *rootIndex, nodeByKey map[uint64]int32, value string) int32 {
	for length := len(value) - 1; length > 0; length-- {
		root := cyclicCoverPrefixRoot(index, value[:length])
		if root == -1 {
			continue
		}

		parent, ok := nodeByKey[cyclicCoverNodeKey(root, length)]
		if ok {
			return parent
		}
	}

	return nodeByKey[cyclicCoverEmptyKey]
}

func cyclicCoverSuffixParent(index *rootIndex, nodeByKey map[uint64]int32, value string) int32 {
	for offset := 1; offset < len(value); offset++ {
		suffix := value[offset:]

		root := cyclicCoverPrefixRoot(index, suffix)
		if root == -1 {
			continue
		}

		parent, ok := nodeByKey[cyclicCoverNodeKey(root, len(suffix))]
		if ok {
			return parent
		}
	}

	return nodeByKey[cyclicCoverEmptyKey]
}

func cyclicCoverPrefixRoot(index *rootIndex, prefix string) int32 {
	if len(prefix) == 0 {
		return 0
	}

	key := getPrefix64(prefix)

	var (
		low  int32
		high int32
	)

	if len(prefix) >= 8 {
		if !index.mayHavePrefix(key) {
			return -1
		}

		low, high = index.prefixRange(key)
	} else {
		low, high = shortPrefixRange(index.buckets, key, len(prefix))
	}

	for low < high {
		middle := low + (high-low)/2
		candidate := index.str(middle)

		if candidate < prefix {
			low = middle + 1
		} else {
			high = middle
		}
	}

	if int(low) >= len(index.roots) || !strings.HasPrefix(index.str(low), prefix) {
		return -1
	}

	return low
}

func cyclicCoverNodeKey(root int32, length int) uint64 {
	return uint64(uint32(root))<<8 | uint64(uint8(length))
}

func exactCyclicCoverOverlapSavings(roots []string) int {
	if len(roots) == 0 {
		return 0
	}

	stateCount := 1 << uint(len(roots))
	scores := make([]int, stateCount)

	for state := range scores {
		scores[state] = -1
	}

	scores[0] = 0

	for state := 0; state < stateCount-1; state++ {
		tail := bits.OnesCount(uint(state))

		score := scores[state]
		if score < 0 {
			continue
		}

		for head := range roots {
			bit := 1 << uint(head)
			if state&bit != 0 {
				continue
			}

			overlap := oracleProperStringOverlap(roots[tail], roots[head], tail == head)
			nextState := state | bit

			scores[nextState] = max(scores[nextState], score+overlap)
		}
	}

	return scores[stateCount-1]
}

func oracleProperStringOverlap(tail, head string, same bool) int {
	maximum := min(len(tail), len(head))

	if same {
		maximum--
	}

	for overlap := maximum; overlap > 0; overlap-- {
		if tail[len(tail)-overlap:] == head[:overlap] {
			return overlap
		}
	}

	return 0
}

func readCyclicCoverCorpus(reader io.Reader) ([]string, error) {
	buffered := bufio.NewReaderSize(reader, 4<<20)

	header := make([]byte, len(cyclicCoverCorpusMagic))

	_, err := io.ReadFull(buffered, header)
	if err != nil {
		return nil, fmt.Errorf("read corpus header: %w", err)
	}

	if string(header) != cyclicCoverCorpusMagic {
		return nil, fmt.Errorf("invalid corpus header %q", header)
	}

	entries := make([]string, 0, 1<<20)

	var value [MaxStringLen]byte

	for index := 0; ; index++ {
		length, readErr := buffered.ReadByte()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return entries, nil
			}

			return nil, fmt.Errorf("read length for string %d: %w", index, readErr)
		}

		bytes := value[:int(length)]

		_, readErr = io.ReadFull(buffered, bytes)
		if readErr != nil {
			return nil, fmt.Errorf("read string %d: %w", index, readErr)
		}

		entries = append(entries, string(bytes))
	}
}
