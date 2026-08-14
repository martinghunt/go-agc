# go-agc
A pure-Go reader for Assembled Genomes Compressor (AGC) v3 archives

This repository is under active development. The implemented metadata reader
can open local files or arbitrary `io.ReaderAt` backends, validate file-format
major version 3, list samples in archive order, identify the reference sample,
and lazily list a sample's contigs. Sequence decoding is the next phase
described in [DESIGN.md](DESIGN.md).

```go
archive, err := agc.Open("genomes.agc")
if err != nil {
	return err
}
defer archive.Close()

samples, err := archive.Samples()
reference, err := archive.ReferenceSample()
contigs, err := archive.Contigs(reference)
```

The implementation is read-only and lazily reads indexed archive parts. It
uses `github.com/klauspost/compress/zstd` and does not require cgo or the AGC
executable. Tests include the upstream toy archive, generated v3 fixtures,
corruption cases, optional reference-CLI cross-checks, and benchmarks.
