package spack_test

import (
	"bufio"
	"math/rand/v2"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/coalaura/spack"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

type resourceMonitor struct {
	stop chan struct{}
	done chan struct{}

	peakAlloc uint64
}

func TestPacker(t *testing.T) {
	printer := message.NewPrinter(language.English)

	t.Log("Reading strings...")

	file, err := os.OpenFile("strings.txt", os.O_RDONLY, 0)
	must(t, err)

	defer file.Close()

	collector := spack.NewStringMap(nil)

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		str := scanner.Text()

		collector.Add(str)
	}

	must(t, scanner.Err())

	t.Logf("Read %s strings (%s bytes)\n", printer.Sprintf("%d", collector.Length()), printer.Sprintf("%d", collector.Size()))

	t.Log("Packing strings...")

	runtime.GC()

	var baseMem runtime.MemStats

	runtime.ReadMemStats(&baseMem)

	monitor := startResourceMonitor(1 * time.Millisecond)

	startTime := time.Now()

	pack, err := collector.Pack()

	duration := time.Since(startTime)
	peakAlloc := monitor.Stop()

	must(t, err)

	t.Logf("Packed strings into %s bytes\n", printer.Sprintf("%d", pack.Len()))

	peakAllocMB := float64(peakAlloc) / 1024 / 1024
	baseAllocMB := float64(baseMem.Alloc) / 1024 / 1024
	addedAllocMB := max(0, peakAllocMB-baseAllocMB)

	t.Logf("Peak Heap Memory: %.2f MB (Baseline: %.2f MB, Net Added: %.2f MB)\n", peakAllocMB, baseAllocMB, addedAllocMB)

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
