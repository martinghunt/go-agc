# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.4.0] - 2026-10-01

### Added
- Add `OpenURL` and configurable `OpenURLWithOptions` for reading AGC v3 archives from HTTP byte-range servers, with strong-ETag pinning, bounded read-ahead caching, concurrent range fetching, and sample-aware coalescing of nearby reference and delta parts.

### Changed
- Prefetch and deduplicate the compressed parts needed by remote sample iteration, downloading small or densely requested objects in full, while leaving local files and ordinary `io.ReaderAt` inputs on the existing demand-driven path.

## [0.3.0] - 2026-09-17

### Added
- Cache decompressed reference and delta segment packs at the archive level, bounded to a fixed byte budget with oldest-first eviction, so repeated `Contig`/`IterateSample`/`IterateAll` calls that share reference groups no longer re-fetch and re-decompress the same data.

### Changed
- Split a cached delta pack into its sequences once instead of rescanning it from the start on every lookup, so a contig with many segments in the same pack no longer costs quadratic time.
- Document the project's development assistance from Claude Code alongside OpenAI Codex.

### Fixed
- Cap sample count and per-sample contig count independently of decompressed stream size, so a small malicious archive can no longer force multi-gigabyte slice/map preallocation before any entry is validated.

## [0.2.0] - 2026-08-19

### Added
- Add `Archive.NewContigReader` and pull-style `ContigReader.Read` for decoding one sample's contigs in archive order with bounded sequence memory.

### Changed
- Cache one sample's named-contig index so repeated `Archive.Contig` lookups avoid rescanning every contig name while preserving first-match behavior.
- Implement `Archive.IterateSample` through the shared pull-style reader path.
- Document the project's development assistance from OpenAI Codex.

## [0.1.0] - 2026-08-14

Release `v0.1.0`, before changelog tracking started in this file.

[Unreleased]: https://github.com/martinghunt/go-agc/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/martinghunt/go-agc/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/martinghunt/go-agc/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/martinghunt/go-agc/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/martinghunt/go-agc/releases/tag/v0.1.0
