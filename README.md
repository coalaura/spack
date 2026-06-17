# spack

spack is a minimal, high-performance string pack library for Go. It compresses a collection of strings into a single, contiguous byte slice using prefix and suffix overlap deduplication.

The resulting blob is flat, coherent and highly optimized for writing to a file and memory mapping (mmap).

## Key Features

* Single Coherent Blob: Packing emits a single byte slice and an array of compact, 5-byte pointers. Perfect for direct disk serialization and zero-copy mmap.
* O(1) Lookups: Retrieving strings is a flat, simple offset lookup.
* Standalone Usability: GetString and GetStringUnsafe are decoupled from any struct. They operate directly on raw byte slices using the 5-byte Pointer, facilitating easy integration with mmap libraries.
* Zero Allocation Options: Unsafe retrieval returns views over the original block using Go string headers to avoid allocation.

## Performance

Packing 81.8 million strings (which originally occupy 3.05 GB of heap space for slice/string headers and characters) takes about 44 seconds.

The process outputs a flat 1.20 GB byte blob, which is a 60.73% reduction in raw string data. Including the compact 5-byte pointers, the total in-memory size is 1.61 GB, yielding an overall 47.30% memory footprint reduction. The compression/packing stage runs with a net peak heap allocation of about 4.4 GB over the dataset baseline.

## Usage

```go
package main

import (
	"fmt"
	"github.com/coalaura/spack"
)

func main() {
	// collect strings
	sm := spack.NewStringMap(nil)

	idx1, _ := sm.Add("hello world")
	idx2, _ := sm.Add("world")

	// pack strings into a flat blob
	packed, err := sm.Pack()
	if err != nil {
		panic(err)
	}

	rawBytes := packed.Bytes()
	pointers := packed.Pointers()

	// independent, O(1) lookups on the raw bytes
	str1, _ := spack.GetStringUnsafe(rawBytes, pointers[idx1])
	str2, _ := spack.GetStringUnsafe(rawBytes, pointers[idx2])

	fmt.Println(str1) // "hello world"
	fmt.Println(str2) // "world"
}
```

## Constraints

Individual strings cannot exceed 255 bytes (MaxStringLen is constrained by the 8-bit pointer length field).