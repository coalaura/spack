package spack_test

import (
	"bufio"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/coalaura/spack"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const SourceFile = "C:\\Users\\Laura\\joaat.sh\\strings.txt"

func TestPacker(t *testing.T) {
	printer := message.NewPrinter(language.English)

	t.Log("Reading strings...")

	file, err := os.OpenFile(SourceFile, os.O_RDONLY, 0)
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

	startTime := time.Now()

	pack, pointers, err := collector.Pack()

	duration := time.Since(startTime)

	must(t, err)

	t.Logf("Packed strings into %s bytes\n", printer.Sprintf("%d", pack.Size()))

	if len(pointers) != collector.Length() {
		t.Fatalf("Expected %s pointers but got %s\n", printer.Sprintf("%d", collector.Length()), printer.Sprintf("%d", len(pointers)))
	}

	t.Log("Testing random read...")

	for range 100 {
		idx := rand.IntN(collector.Length())

		expected := collector.At(idx)
		pointer := pointers[idx]

		actual, err := pack.Get(pointer)
		must(t, err)

		if actual != expected {
			t.Fatalf("Expected %q at index %d but got %q\n", expected, idx, actual)
		}
	}

	score := (1.0 - (float64(pack.Size()) / float64(collector.Size()))) * 100.0

	t.Logf("Final compression ratio is %.4f%% in %s.\n", score, duration.Round(time.Millisecond))
}

func must(t *testing.T, err error) {
	if err == nil {
		return
	}

	t.Fatal(err)
}
