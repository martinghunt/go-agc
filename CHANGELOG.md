# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed
- Document the project's development assistance from Claude Code alongside OpenAI Codex.

## [0.2.0] - 2026-08-19

### Added
- Add `Archive.NewContigReader` and pull-style `ContigReader.Read` for decoding one sample's contigs in archive order with bounded sequence memory.

### Changed
- Cache one sample's named-contig index so repeated `Archive.Contig` lookups avoid rescanning every contig name while preserving first-match behavior.
- Implement `Archive.IterateSample` through the shared pull-style reader path.
- Document the project's development assistance from OpenAI Codex.

## [0.1.0] - 2026-08-14

Release `v0.1.0`, before changelog tracking started in this file.

[Unreleased]: https://github.com/martinghunt/go-agc/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/martinghunt/go-agc/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/martinghunt/go-agc/releases/tag/v0.1.0
