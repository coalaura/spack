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

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			runtime.ReadMemStats(&memStats)

			if memStats.Alloc > m.peakAlloc {
				m.peakAlloc = memStats.Alloc
			}
		}
	}
}

func (m *resourceMonitor) Stop() uint64 {
	close(m.stop)
	<-m.done

	return m.peakAlloc
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

	err = readCorpus(file, collector)
	must(t, err)

	t.Logf("Read %s strings (%s bytes)\n", printer.Sprintf("%d", collector.Length()), printer.Sprintf("%d", collector.Size()))

	t.Log("Packing strings...")

	measureMemory := os.Getenv("SPACK_TEST_MEMORY") == "1"

	var (
		baseMem runtime.MemStats
		monitor *resourceMonitor

		peakAlloc uint64
	)

	if measureMemory {
		runtime.GC()

		runtime.ReadMemStats(&baseMem)

		monitor = startResourceMonitor(1 * time.Millisecond)
	}

	startTime := time.Now()

	pack, err := collector.Pack()

	duration := time.Since(startTime)

	if measureMemory {
		peakAlloc = monitor.Stop()
	}

	must(t, err)

	t.Logf("Packed strings into %s bytes, %s bytes in memory\n", printer.Sprintf("%d", pack.Len()), printer.Sprintf("%d", pack.Size()))

	if measureMemory {
		peakAllocMB := float64(peakAlloc) / 1024 / 1024
		baseAllocMB := float64(baseMem.Alloc) / 1024 / 1024
		addedAllocMB := max(0, peakAllocMB-baseAllocMB)

		t.Logf("Peak Heap Memory: %.2f MB (Baseline: %.2f MB, Net Added: %.2f MB)\n", peakAllocMB, baseAllocMB, addedAllocMB)
	}

	pointers := pack.Pointers()

	if len(pointers) != collector.Length() {
		t.Fatalf("Expected %s pointers but got %s\n", printer.Sprintf("%d", collector.Length()), printer.Sprintf("%d", len(pointers)))
	}

	t.Log("Testing random read...")

	for range 4096 {
		idx := rand.IntN(collector.Length())

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

func readCorpus(r io.Reader, collector *spack.StringMap) error {
	reader := bufio.NewReaderSize(r, 4<<20)

	header := make([]byte, len(corpusMagic))

	_, err := io.ReadFull(reader, header)
	if err != nil {
		return fmt.Errorf("read corpus header: %w", err)
	}

	if string(header) != corpusMagic {
		return fmt.Errorf("invalid corpus header %q", header)
	}

	var value [spack.MaxStringLen]byte

	for index := 0; ; index++ {
		length, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("read length for string %d: %w", index, err)
		}

		buf := value[:int(length)]

		_, err = io.ReadFull(reader, buf)
		if err != nil {
			return fmt.Errorf("read string %d: %w", index, err)
		}

		_, err = collector.Add(string(buf))
		if err != nil {
			return fmt.Errorf("add string %d: %w", index, err)
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
