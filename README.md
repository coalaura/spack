```
pack_test.go:27: Reading strings...
pack_test.go:46: Read 81,845,115 strings (3,047,871,657 bytes)
pack_test.go:48: Packing strings...
pack_test.go:67: Packed strings into 1,197,014,423 bytes
pack_test.go:73: Peak Heap Memory: 13398.02 MB (Baseline: 3420.30 MB, Net Added: 9977.72 MB)
pack_test.go:79: Testing random read...
pack_test.go:106: Final compression ratios (in 47.549s):
pack_test.go:107: - strings (no pointers): 60.73%
pack_test.go:108: - total (not padded)   : 47.30%
pack_test.go:109: - total (padded)       : 39.24%
```