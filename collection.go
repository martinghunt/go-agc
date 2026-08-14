package agc

// Collection decoding here is derived from REFRESH Bioinformatics Group's
// AGC collection_v3.cpp, distributed under the MIT license. See
// THIRD_PARTY_NOTICES.md and third_party/agc.LICENSE.

import (
	"fmt"

	"github.com/klauspost/compress/zstd"
)

const maxSampleMetadataSize = 256 << 20

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
	decoder, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(maxSampleMetadataSize),
		zstd.WithDecodeAllCapLimit(true),
	)
	if err != nil {
		return fmt.Errorf("agc: initialize zstd decoder: %w", err)
	}
	defer decoder.Close()
	raw, err := decoder.DecodeAll(packed, make([]byte, 0, int(rawSize)))
	if err != nil {
		return fmt.Errorf("%w: decompress collection-samples: %v", ErrCorruptArchive, err)
	}
	if uint64(len(raw)) != rawSize {
		return fmt.Errorf("%w: collection-samples decoded size %d, want %d", ErrCorruptArchive, len(raw), rawSize)
	}
	c := byteCursor{data: raw}
	count, err := c.collectionUint()
	if err != nil || uint64(count) > uint64(len(raw)) {
		return fmt.Errorf("%w: invalid sample count", ErrCorruptArchive)
	}
	samples := make([]Sample, 0, count)
	seen := make(map[string]struct{}, count)
	for i := uint32(0); i < count; i++ {
		name, err := c.cstring()
		if err != nil || name == "" {
			return fmt.Errorf("%w: invalid sample name at index %d", ErrCorruptArchive, i)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("%w: duplicate sample %q", ErrCorruptArchive, name)
		}
		seen[name] = struct{}{}
		samples = append(samples, Sample{Name: name})
	}
	if !c.empty() {
		return fmt.Errorf("%w: trailing collection-samples data", ErrCorruptArchive)
	}
	a.samples = samples
	return nil
}
