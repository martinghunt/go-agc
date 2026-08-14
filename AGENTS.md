# go-agc contributor instructions

## Scope

- Build a read-only, pure-Go library for AGC file-format major version 3.
- Reject every other file-format major version. Do not add compatibility shims for v1, v2, or a future v4.
- Local seekable files are the primary input, but decoding internals must depend on `io.ReaderAt` and an explicit size.
- Do not implement archive creation, append, or mutation.

## Design rules

- Decode lazily. Opening may read the footer, version metadata, and the sample-name index; sequence and contig-detail streams are loaded only when requested.
- Preserve sample and contig identity separately. Never make `contig@sample` the internal identity or the sole public lookup key.
- Bound every offset, length, count, and decompressed size before allocation or slicing. Wrap structural failures with `ErrCorruptArchive`.
- Use a pure-Go zstd implementation. No cgo and no shelling out to the AGC executable in library code.
- Keep the public API small and read-only. New exported identifiers require documentation and an end-to-end use case.
- Code ported or closely derived from upstream AGC must retain the upstream MIT attribution in the source file and in `THIRD_PARTY_NOTICES.md`.

## Tests and workflow

- Work BDD/TDD: add a failing behavior test before its implementation, then keep the smallest implementation that makes it pass.
- Express behavior tests with Given/When/Then structure (test names, subtests, or comments).
- Use the upstream `toy_ex/toy_ex.agc` archive plus generated v3 fixtures covering boundaries and corruption.
- Cross-check fixture output against the reference AGC executable. Reference-CLI checks may be opt-in when the executable is unavailable.
- Every parser change needs malformed/truncated-input tests. Every completed vertical slice needs a benchmark for its primary operation.
- Run `go test ./...`, `go test -race ./...`, and relevant benchmarks before completing a phase.

