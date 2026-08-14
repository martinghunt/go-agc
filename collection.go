package agc

// Collection decoding here is derived from REFRESH Bioinformatics Group's
// AGC collection_v3.cpp, distributed under the MIT license. See
// THIRD_PARTY_NOTICES.md and third_party/agc.LICENSE.

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	maxSampleMetadataSize = 256 << 20
	maxContigBatchSize    = 1 << 30
	maxParamsSize         = 64
)

// Samples returns samples in archive order. The reference sample is first.
func (a *Archive) Samples() ([]Sample, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, ErrClosed
	}
	if a.samples == nil {
		if err := a.loadSamples(); err != nil {
			return nil, err
		}
	}
	return append([]Sample(nil), a.samples...), nil
}

// ReferenceSample returns the first sample stored in the v3 collection.
func (a *Archive) ReferenceSample() (Sample, error) {
	samples, err := a.Samples()
	if err != nil {
		return Sample{}, err
	}
	if len(samples) == 0 {
		return Sample{}, fmt.Errorf("%w: archive has no samples", ErrCorruptArchive)
	}
	return samples[0], nil
}

// Contigs returns a sample's contigs in archive order. Each result retains its
// sample identity separately from its stored contig name.
func (a *Archive) Contigs(sample Sample) ([]ContigInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, ErrClosed
	}
	if a.samples == nil {
		if err := a.loadSamples(); err != nil {
			return nil, err
		}
	}
	sampleID, ok := a.sampleIDs[sample.Name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSampleNotFound, sample.Name)
	}
	if err := a.loadParams(); err != nil {
		return nil, err
	}
	batchID := sampleID / int(a.batchSize)
	if a.contigBatchID != batchID {
		if err := a.loadContigBatch(batchID); err != nil {
			return nil, err
		}
	}
	withinBatch := sampleID - batchID*int(a.batchSize)
	if withinBatch < 0 || withinBatch >= len(a.contigBatch) {
		return nil, fmt.Errorf("%w: sample %q missing from contig batch", ErrCorruptArchive, sample.Name)
	}
	names := a.contigBatch[withinBatch]
	contigs := make([]ContigInfo, len(names))
	canonicalSample := a.samples[sampleID]
	for i, name := range names {
		contigs[i] = ContigInfo{Sample: canonicalSample, Name: name}
	}
	return contigs, nil
}

func (a *Archive) loadSamples() error {
	part, err := a.index.part("collection-samples", 0)
	if err != nil {
		return err
	}
	packed, rawSize, err := readPart(a.r, a.index.dataEnd, part, maxSampleMetadataSize)
	if err != nil {
		return fmt.Errorf("collection-samples: %w", err)
	}
	if rawSize == 0 || rawSize > maxSampleMetadataSize {
		return fmt.Errorf("%w: invalid collection-samples raw size %d", ErrCorruptArchive, rawSize)
	}
	raw, err := decodeZstdBlock(packed, rawSize, maxSampleMetadataSize)
	if err != nil {
		return fmt.Errorf("collection-samples: %w", err)
	}
	c := byteCursor{data: raw}
	count, err := c.collectionUint()
	if err != nil || uint64(count) > uint64(len(raw)) {
		return fmt.Errorf("%w: invalid sample count", ErrCorruptArchive)
	}
	samples := make([]Sample, 0, count)
	seen := make(map[string]struct{}, count)
	sampleIDs := make(map[string]int, count)
	for i := uint32(0); i < count; i++ {
		name, err := c.cstring()
		if err != nil || name == "" {
			return fmt.Errorf("%w: invalid sample name at index %d", ErrCorruptArchive, i)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("%w: duplicate sample %q", ErrCorruptArchive, name)
		}
		seen[name] = struct{}{}
		sampleIDs[name] = len(samples)
		samples = append(samples, Sample{Name: name})
	}
	if !c.empty() {
		return fmt.Errorf("%w: trailing collection-samples data", ErrCorruptArchive)
	}
	a.samples = samples
	a.sampleIDs = sampleIDs
	return nil
}

func (a *Archive) loadParams() error {
	if a.paramsLoaded {
		return nil
	}
	part, err := a.index.part("params", 0)
	if err != nil {
		return err
	}
	data, _, err := readPart(a.r, a.index.dataEnd, part, maxParamsSize)
	if err != nil {
		return fmt.Errorf("params: %w", err)
	}
	if len(data) != 16 {
		return fmt.Errorf("%w: v3 params size %d, want 16", ErrCorruptArchive, len(data))
	}
	batchSize := binary.LittleEndian.Uint32(data[8:12])
	maxInt := uint64(^uint(0) >> 1)
	if batchSize == 0 || uint64(batchSize) > maxInt {
		return fmt.Errorf("%w: invalid sample batch cardinality %d", ErrCorruptArchive, batchSize)
	}
	a.batchSize = batchSize
	a.paramsLoaded = true
	return nil
}

func (a *Archive) loadContigBatch(batchID int) error {
	part, err := a.index.part("collection-contigs", batchID)
	if err != nil {
		return err
	}
	packed, rawSize, err := readPart(a.r, a.index.dataEnd, part, maxContigBatchSize)
	if err != nil {
		return fmt.Errorf("collection-contigs part %d: %w", batchID, err)
	}
	raw, err := decodeZstdBlock(packed, rawSize, maxContigBatchSize)
	if err != nil {
		return fmt.Errorf("collection-contigs part %d: %w", batchID, err)
	}
	c := byteCursor{data: raw}
	count, err := c.collectionUint()
	batchStart := batchID * int(a.batchSize)
	if batchStart < 0 || batchStart >= len(a.samples) {
		return fmt.Errorf("%w: invalid contig batch %d", ErrCorruptArchive, batchID)
	}
	expected := len(a.samples) - batchStart
	if expected > int(a.batchSize) {
		expected = int(a.batchSize)
	}
	if err != nil || int(count) != expected {
		return fmt.Errorf("%w: contig batch %d has %d samples, want %d", ErrCorruptArchive, batchID, count, expected)
	}
	batch := make([][]string, int(count))
	for i := range batch {
		contigCount, err := c.collectionUint()
		if err != nil || uint64(contigCount) > uint64(c.remaining()) {
			return fmt.Errorf("%w: invalid contig count in batch %d sample %d", ErrCorruptArchive, batchID, i)
		}
		batch[i] = make([]string, int(contigCount))
		var previous []string
		for j := range batch[i] {
			encoded, err := c.cstring()
			if err != nil {
				return fmt.Errorf("%w: invalid contig name in batch %d sample %d", ErrCorruptArchive, batchID, i)
			}
			name, components, err := decodeContigName(previous, encoded)
			if err != nil {
				return fmt.Errorf("batch %d sample %d contig %d: %w", batchID, i, j, err)
			}
			batch[i][j] = name
			previous = components
		}
	}
	if !c.empty() {
		return fmt.Errorf("%w: trailing collection-contigs data in batch %d", ErrCorruptArchive, batchID)
	}
	a.contigBatch = batch
	a.contigBatchID = batchID
	return nil
}

func decodeContigName(previous []string, encoded string) (string, []string, error) {
	current := strings.Split(encoded, " ")
	if len(current) != len(previous) {
		return encoded, current, nil
	}
	decoded := make([]string, len(current))
	for i, component := range current {
		bytes := []byte(component)
		if len(bytes) == 1 && bytes[0] == 0x81 {
			decoded[i] = previous[i]
			continue
		}
		prior := []byte(previous[i])
		out := make([]byte, 0, len(prior))
		priorPos := 0
		for _, b := range bytes {
			if b < 0x80 {
				out = append(out, b)
				priorPos++
				continue
			}
			repeated := 256 - int(b)
			if priorPos+repeated > len(prior) {
				return "", nil, fmt.Errorf("%w: contig-name repetition exceeds prior component", ErrCorruptArchive)
			}
			out = append(out, prior[priorPos:priorPos+repeated]...)
			priorPos += repeated
		}
		decoded[i] = string(out)
	}
	return strings.Join(decoded, " "), decoded, nil
}

func decodeZstdBlock(packed []byte, rawSize, maxSize uint64) ([]byte, error) {
	if rawSize == 0 || rawSize > maxSize {
		return nil, fmt.Errorf("%w: invalid decoded size %d", ErrCorruptArchive, rawSize)
	}
	decoder, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(maxSize),
		zstd.WithDecodeAllCapLimit(true),
	)
	if err != nil {
		return nil, fmt.Errorf("agc: initialize zstd decoder: %w", err)
	}
	defer decoder.Close()
	raw, err := decoder.DecodeAll(packed, make([]byte, 0, int(rawSize)))
	if err != nil {
		return nil, fmt.Errorf("%w: decompress zstd block: %v", ErrCorruptArchive, err)
	}
	if uint64(len(raw)) != rawSize {
		return nil, fmt.Errorf("%w: decoded size %d, want %d", ErrCorruptArchive, len(raw), rawSize)
	}
	return raw, nil
}
