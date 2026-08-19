# go-agc

A pure-Go reader for [Assembled Genomes Compressor (AGC)](https://github.com/refresh-bio/agc)
v3 archives.

This repository was developed with substantial coding assistance from [OpenAI Codex](https://openai.com/codex), which helped with implementation, refactoring, tests, documentation, and benchmarking under human direction and review.

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

reader, err := archive.NewContigReader(reference)
for {
	contig, err := reader.Read()
	if errors.Is(err, io.EOF) {
		break
	}
	if err != nil {
		return err
	}
	consume(contig.Name, contig.Sequence)
}

err = archive.IterateSample(reference, func(contig agc.Contig) error {
	return consume(contig.Name, contig.Sequence)
})
```

The implementation is read-only and lazily reads indexed archive parts. It
uses `github.com/klauspost/compress/zstd` and does not require cgo or the AGC
executable. Tests include the upstream toy archive, generated v3 fixtures,
corruption cases, optional reference-CLI cross-checks, and benchmarks.

## Releases

This is a library-only Go module, so releases are semantic-version Git tags;
there are no executable artifacts to build. See [RELEASING.md](RELEASING.md)
for the tagging workflow and consumer installation command.
