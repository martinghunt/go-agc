# Design and implementation plan

## Proposed package API

```go
package agc

type Sample struct {
	Name string
}

type ContigInfo struct {
	Sample Sample
	Name   string
}

type Contig struct {
	Sample   Sample
	Name     string
	Sequence []byte
}

func Open(path string) (*Archive, error)
func OpenReaderAt(r io.ReaderAt, size int64) (*Archive, error)
func (a *Archive) Close() error
func (a *Archive) Samples() ([]Sample, error)
func (a *Archive) ReferenceSample() (Sample, error)
func (a *Archive) Contigs(sample Sample) ([]ContigInfo, error)
func (a *Archive) Contig(sample Sample, name string) (Contig, error)
func (a *Archive) IterateSample(sample Sample, yield func(Contig) error) error
func (a *Archive) IterateAll(yield func(Contig) error) error
```

`OpenReaderAt` does not take ownership of its reader; `Close` only closes resources opened by `Open`. Sample enumeration and iteration use archive order, which keeps the reference sample first. Returned names remain exactly as stored. A contig is always selected by separate sample and contig values, including when names collide between samples.

The callback iterators avoid forcing all decoded sequence data into memory and allow callers to stop by returning an error. The API may gain `context.Context` variants later only if decompression cancellation proves useful.

## Internal shape

The footer is parsed into a small immutable stream/part index. Parts are fetched with `io.ReaderAt`, validating the metadata prefix and all bounds. Collection sample names are cached after first use. Contig-name/detail batches and segment packs will use bounded caches rather than whole-archive prefetching.

## Phases

1. **Open and identify — complete.** Parse and validate the footer, load `file_type_info`, reject non-v3 archives, lazily decode `collection-samples`, expose open/close, samples, and reference sample. Add toy-archive behavior tests, corruption cases, CLI cross-check, and open/list benchmarks.
2. **Contig catalogue — complete.** Decode v3 contig-name batches on demand; expose `Contigs`; test delta-encoded names, batch boundaries, duplicate names across samples, and corrupt zstd/collection data.
3. **Sequence primitives — complete.** Port v3 segment-group reference/delta decoding, DNA symbol conversion, reverse complement, and overlap assembly. Test generated fixtures against reference AGC output.
4. **Retrieval — complete.** Expose `Contig`, retaining full stored header while matching the upstream short-name rules explicitly. Add missing-name behavior and corruption coverage.
5. **Streaming iteration — complete.** Add one-sample and all-sample callback iteration with bounded working memory and deterministic archive order.
6. **Hardening and performance — complete for the initial reader.** Expand malformed-input/fuzz coverage, enforce allocation limits, race-test concurrent reads, and benchmark metadata, random contig lookup, and full traversal.
