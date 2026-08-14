# go-agc
A pure-Go reader for Assembled Genomes Compressor (AGC) v3 archives

The read-only library opens local files or arbitrary `io.ReaderAt` backends,
validates file-format major version 3, lists samples and contigs, retrieves
named contigs, and iterates one sample or the complete archive. Metadata and
sequence streams are decoded lazily with bounded working memory. Other AGC
file-format major versions are intentionally rejected.

```go
archive, err := agc.Open("genomes.agc")
if err != nil {
	return err
}
defer archive.Close()

samples, err := archive.Samples()
reference, err := archive.ReferenceSample()
contigs, err := archive.Contigs(reference)
err = archive.IterateSample(reference, func(contig agc.Contig) error {
	return consume(contig.Name, contig.Sequence)
})
```

The implementation is read-only and lazily reads indexed archive parts. It
uses `github.com/klauspost/compress/zstd` and does not require cgo or the AGC
executable. Tests include the upstream toy archive, generated v3 fixtures,
corruption cases, optional reference-CLI cross-checks, and benchmarks.
