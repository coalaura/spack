package spack_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/coalaura/spack"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const corpusMagic = "SPACKC01"

type resourceMonitor struct {
	stop chan struct{}
	done chan struct{}

	peakAlloc uint64
}

func (m *resourceMonitor) run(interval time.Duration) {
	defer close(m.done)

	var memStats runtime.MemStats

	m.sample(&memStats)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			m.sample(&memStats)

			return
		case <-ticker.C:
			m.sample(&memStats)
		}
	}
}

func (m *resourceMonitor) Stop() uint64 {
	close(m.stop)
	<-m.done

	return m.peakAlloc
}

func (m *resourceMonitor) sample(memStats *runtime.MemStats) {
	runtime.ReadMemStats(memStats)

	if memStats.Alloc > m.peakAlloc {
		m.peakAlloc = memStats.Alloc
	}
}

func TestPacker(t *testing.T) {
	corpusPath := os.Getenv("SPACK_TEST_CORPUS_PATH")
	if corpusPath == "" {
		t.Skip("set SPACK_TEST_CORPUS_PATH to a .spc corpus file")
	}

	if filepath.Ext(corpusPath) != ".spc" {
		t.Fatalf("Corpus file must use the .spc extension: %q\n", corpusPath)
	}

	printer := message.NewPrinter(language.English)

	t.Logf("Reading corpus %q...\n", corpusPath)

	file, err := os.Open(corpusPath)
	must(t, err)

	t.Cleanup(func() {
		must(t, file.Close())
	})

	collector := spack.NewStringMap(nil)

	payloadBytes, err := readCorpus(file, collector)
	must(t, err)

	t.Logf("Read %s strings (%s raw payload bytes, %s logical bytes)\n", printer.Sprintf("%d", collector.Length()), printer.Sprintf("%d", payloadBytes), printer.Sprintf("%d", collector.Size()))

	t.Log("Packing strings...")

	measureMemory := os.Getenv("SPACK_TEST_MEMORY") == "1"
	measureBound := os.Getenv("SPACK_TEST_BOUND") == "1"
	disableGC := os.Getenv("SPACK_TEST_DISABLE_GC") == "1"

	options := spack.PackOptions{
		DisableGC: disableGC,
	}

	t.Logf("Settings: Pointer32, GOMAXPROCS=%d, DisableGC=%t, %s/%s, %s\n", runtime.GOMAXPROCS(0), disableGC, runtime.GOOS, runtime.GOARCH, runtime.Version())

	var (
		baseMem runtime.MemStats
		monitor *resourceMonitor

		bound     spack.BlobSizeBound
		peakAlloc uint64
	)

	if measureMemory {
		runtime.GC()

		runtime.ReadMemStats(&baseMem)

		monitor = startResourceMonitor(1 * time.Millisecond)
	}

	startTime := time.Now()

	var pack *spack.PackedBlob[spack.Pointer32]

	if measureBound {
		pack, err = collector.PackWithBlobSizeBound[spack.Pointer32](&bound, options)
	} else {
		pack, err = collector.Pack[spack.Pointer32](options)
	}

	duration := time.Since(startTime)

	if measureMemory {
		peakAlloc = monitor.Stop()
	}

	must(t, err)

	pointerBytes := pack.Size() - pack.Len()

	t.Logf("Packed strings into %s blob bytes + %s pointer bytes = %s total bytes\n", printer.Sprintf("%d", pack.Len()), printer.Sprintf("%d", pointerBytes), printer.Sprintf("%d", pack.Size()))

	if measureBound {
		remainingSaving := bound.CurrentBlobBytes - bound.LowerBoundBytes

		var remainingPercent float64

		if bound.CurrentBlobBytes != 0 {
			remainingPercent = float64(remainingSaving) / float64(bound.CurrentBlobBytes) * 100
		}

		t.Logf("Final roots: %s roots, %s bytes, longest %s bytes\n", printer.Sprintf("%d", bound.RootCount), printer.Sprintf("%d", bound.RootBytes), printer.Sprintf("%d", bound.LongestRootBytes))
		t.Logf("Certified blob interval: [%s, %s] bytes\n", printer.Sprintf("%d", bound.LowerBoundBytes), printer.Sprintf("%d", bound.CurrentBlobBytes))
		t.Logf("Maximum possible remaining saving: %s bytes (%.6f%%)\n", printer.Sprintf("%d", remainingSaving), remainingPercent)
		t.Logf("Overlap upper bounds: outgoing %s, incoming %s, used %s bytes\n", printer.Sprintf("%d", bound.OutgoingOverlapUpperBound), printer.Sprintf("%d", bound.IncomingOverlapUpperBound), printer.Sprintf("%d", bound.OverlapUpperBound))
		t.Logf("Roots without positive overlap: outgoing %s, incoming %s\n", printer.Sprintf("%d", bound.RootsWithoutOutgoingOverlap), printer.Sprintf("%d", bound.RootsWithoutIncomingOverlap))
		t.Logf("Outgoing maximum-overlap distribution: %s\n", overlapDistribution(bound.OutgoingMaximumDistribution, printer))
		t.Logf("Incoming maximum-overlap distribution: %s\n", overlapDistribution(bound.IncomingMaximumDistribution, printer))
		t.Logf("Timing: diagnostic-enabled Pack call %.3f s; bound calculation within that call %.3f s\n", duration.Seconds(), bound.ComputationTime.Seconds())
	} else {
		t.Logf("Timing: ordinary Pack call %.3f s\n", duration.Seconds())
	}

	if measureMemory {
		peakAllocMB := float64(peakAlloc) / 1024 / 1024
		baseAllocMB := float64(baseMem.Alloc) / 1024 / 1024
		t.Logf("Sampled peak Go heap allocation during Pack: %.2f MiB (pre-call Go heap allocation: %.2f MiB; 1 ms samples)\n", peakAllocMB, baseAllocMB)
	}

	pointers := pack.Pointers()

	if len(pointers) != collector.Length() {
		t.Fatalf("Expected %s pointers but got %s\n", printer.Sprintf("%d", collector.Length()), printer.Sprintf("%d", len(pointers)))
	}

	t.Log("Testing random read...")

	rng := rand.New(rand.NewPCG(0x4a6f, 0x9c21))

	for range 4096 {
		idx := rng.IntN(collector.Length())

		expected := collector.GetString(idx)
		pointer := pointers[idx]

		actual, err := pack.GetStringUnsafe(pointer)
		must(t, err)

		if actual != expected {
			t.Fatalf("Expected %q at index %d but got %q\n", expected, idx, actual)
		}
	}

	stringScore := (1.0 - (float64(pack.Len()) / float64(collector.Size()))) * 100.0
	totalScoreA := (1.0 - (float64(pack.Size()) / float64(collector.Size()))) * 100.0

	t.Logf("Final compression ratios (in %s):\n", duration.Round(time.Millisecond))
	t.Logf("- strings (no pointers): %.2f%%\n", stringScore)
	t.Logf("- total (with pointers): %.2f%%\n", totalScoreA)
}

func readCorpus(r io.Reader, collector *spack.StringMap) (uint64, error) {
	reader := bufio.NewReaderSize(r, 4<<20)

	header := make([]byte, len(corpusMagic))

	_, err := io.ReadFull(reader, header)
	if err != nil {
		return 0, fmt.Errorf("read corpus header: %w", err)
	}

	if string(header) != corpusMagic {
		return 0, fmt.Errorf("invalid corpus header %q", header)
	}

	var (
		value        [spack.MaxStringLen]byte
		payloadBytes uint64
	)

	for index := 0; ; index++ {
		length, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return payloadBytes, nil
			}

			return 0, fmt.Errorf("read length for string %d: %w", index, err)
		}

		buf := value[:int(length)]
		payloadBytes += uint64(length)

		_, err = io.ReadFull(reader, buf)
		if err != nil {
			return 0, fmt.Errorf("read string %d: %w", index, err)
		}

		_, err = collector.Add(string(buf))
		if err != nil {
			return 0, fmt.Errorf("add string %d: %w", index, err)
		}
	}
}

func must(t *testing.T, err error) {
	if err == nil {
		return
	}

	t.Fatal(err)
}

func startResourceMonitor(interval time.Duration) *resourceMonitor {
	m := &resourceMonitor{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}

	go m.run(interval)

	return m
}

func overlapDistribution(distribution [spack.MaxStringLen + 1]uint64, printer *message.Printer) string {
	buffer := make([]byte, 0, 256)

	for overlap, count := range distribution {
		if count == 0 {
			continue
		}

		if len(buffer) != 0 {
			buffer = append(buffer, ' ')
		}

		entry := printer.Sprintf("%d:%d", overlap, count)

		buffer = append(buffer, entry...)
	}

	return string(buffer)
}
