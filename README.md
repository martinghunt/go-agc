# go-agc
A pure-Go reader for Assembled Genomes Compressor (AGC) v3 archives

This repository is under active development. The first tested vertical slice
can open local files or arbitrary `io.ReaderAt` backends, validate file-format
major version 3, list samples in archive order, and identify the reference
sample. Contig catalogue and sequence decoding are the next phases described
in [DESIGN.md](DESIGN.md).

```go
archive, err := agc.Open("genomes.agc")
if err != nil {
	return err
}
defer archive.Close()

samples, err := archive.Samples()
reference, err := archive.ReferenceSample()
```

The implementation is read-only and lazily reads indexed archive parts. It
uses `github.com/klauspost/compress/zstd` and does not require cgo or the AGC
executable. Tests include the upstream toy archive, generated v3 fixtures,
corruption cases, optional reference-CLI cross-checks, and benchmarks.
