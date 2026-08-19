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
	maxDetailsBatchSize   = 256 << 20
	maxParamsSize         = 64
	maxSegmentsPerBatch   = 1 << 24
)

type segmentDescriptor struct {
	groupID   uint32
	inGroupID uint32
	reversed  bool
	rawLength uint32
}

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
	sampleID, names, err := a.contigNamesLocked(sample)
	if err != nil {
		return nil, err
	}
	contigs := make([]ContigInfo, len(names))
	canonicalSample := a.samples[sampleID]
	for i, name := range names {
		contigs[i] = ContigInfo{Sample: canonicalSample, Name: name}
	}
	return contigs, nil
}

func (a *Archive) contigCount(sample Sample) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, names, err := a.contigNamesLocked(sample)
	if err != nil {
		return 0, err
	}
	return len(names), nil
}

func (a *Archive) contigNamesLocked(sample Sample) (int, []string, error) {
	if a.closed {
		return 0, nil, ErrClosed
	}
	if a.samples == nil {
		if err := a.loadSamples(); err != nil {
			return 0, nil, err
		}
	}
	sampleID, ok := a.sampleIDs[sample.Name]
	if !ok {
		return 0, nil, fmt.Errorf("%w: %q", ErrSampleNotFound, sample.Name)
	}
	if err := a.loadParams(); err != nil {
		return 0, nil, err
	}
	batchID := sampleID / int(a.batchSize)
	if a.contigBatchID != batchID {
		if err := a.loadContigBatch(batchID); err != nil {
			return 0, nil, err
		}
	}
	withinBatch := sampleID - batchID*int(a.batchSize)
	if withinBatch < 0 || withinBatch >= len(a.contigBatch) {
		return 0, nil, fmt.Errorf("%w: sample %q missing from contig batch", ErrCorruptArchive, sample.Name)
	}
	return sampleID, a.contigBatch[withinBatch], nil
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
	a.kmerLength = binary.LittleEndian.Uint32(data[0:4])
	a.minMatchLen = binary.LittleEndian.Uint32(data[4:8])
	a.segmentSize = binary.LittleEndian.Uint32(data[12:16])
	if a.minMatchLen == 0 || uint64(a.segmentSize)+uint64(a.kmerLength) > uint64(^uint32(0)) {
		return fmt.Errorf("%w: invalid compression parameters", ErrCorruptArchive)
	}
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
	a.contigDetails = nil
	a.detailsBatchID = -1
	return nil
}

func (a *Archive) loadContigDetails(batchID int) error {
	if a.detailsBatchID == batchID {
		return nil
	}
	if a.contigBatchID != batchID {
		if err := a.loadContigBatch(batchID); err != nil {
			return err
		}
	}
	part, err := a.index.part("collection-details", batchID)
	if err != nil {
		return err
	}
	stream, _, err := readPart(a.r, a.index.dataEnd, part, maxDetailsBatchSize)
	if err != nil {
		return fmt.Errorf("collection-details part %d: %w", batchID, err)
	}
	c := byteCursor{data: stream}
	type blockSize struct{ raw, packed uint32 }
	var sizes [5]blockSize
	var totalPacked uint64
	for i := range sizes {
		sizes[i].raw, err = c.collectionUint()
		if err != nil {
			return fmt.Errorf("%w: invalid collection-details size header", ErrCorruptArchive)
		}
		sizes[i].packed, err = c.collectionUint()
		totalPacked += uint64(sizes[i].packed)
		if err != nil || totalPacked > uint64(c.remaining()) {
			return fmt.Errorf("%w: invalid collection-details packed sizes", ErrCorruptArchive)
		}
	}
	if totalPacked != uint64(c.remaining()) {
		return fmt.Errorf("%w: collection-details packed sizes do not fill part", ErrCorruptArchive)
	}
	var blocks [5][]byte
	for i, size := range sizes {
		packed := c.data[c.pos : c.pos+int(size.packed)]
		c.pos += int(size.packed)
		if size.raw == 0 {
			return fmt.Errorf("%w: empty collection-details block %d", ErrCorruptArchive, i)
		}
		blocks[i], err = decodeZstdBlock(packed, uint64(size.raw), maxDetailsBatchSize)
		if err != nil {
			return fmt.Errorf("collection-details block %d: %w", i, err)
		}
	}

	shape := byteCursor{data: blocks[0]}
	sampleCount, err := shape.collectionUint()
	if err != nil || int(sampleCount) != len(a.contigBatch) {
		return fmt.Errorf("%w: collection-details sample count %d, want %d", ErrCorruptArchive, sampleCount, len(a.contigBatch))
	}
	details := make([][][]segmentDescriptor, len(a.contigBatch))
	var segmentCount64 uint64
	for i := range details {
		contigCount, err := shape.collectionUint()
		if err != nil || int(contigCount) != len(a.contigBatch[i]) {
			return fmt.Errorf("%w: collection-details contig count mismatch", ErrCorruptArchive)
		}
		details[i] = make([][]segmentDescriptor, int(contigCount))
		for j := range details[i] {
			count, err := shape.collectionUint()
			segmentCount64 += uint64(count)
			if err != nil || count == 0 || segmentCount64 > maxSegmentsPerBatch {
				return fmt.Errorf("%w: invalid segment count in details batch", ErrCorruptArchive)
			}
			details[i][j] = make([]segmentDescriptor, int(count))
		}
	}
	if !shape.empty() {
		return fmt.Errorf("%w: trailing collection-details shape data", ErrCorruptArchive)
	}
	segmentCount := int(segmentCount64)

	values := make([][]uint32, 5)
	for blockID := 1; blockID < 5; blockID++ {
		cursor := byteCursor{data: blocks[blockID]}
		values[blockID] = make([]uint32, segmentCount)
		for i := range values[blockID] {
			values[blockID][i], err = cursor.collectionUint()
			if err != nil {
				return fmt.Errorf("%w: truncated collection-details block %d", ErrCorruptArchive, blockID)
			}
		}
		if !cursor.empty() {
			return fmt.Errorf("%w: trailing collection-details block %d", ErrCorruptArchive, blockID)
		}
	}

	previousIDs := make(map[uint32]int64)
	predictedLength := uint64(a.segmentSize) + uint64(a.kmerLength)
	item := 0
	for i := range details {
		for j := range details[i] {
			for k := range details[i][j] {
				groupID := values[1][item]
				previous, exists := previousIDs[groupID]
				if !exists {
					previous = -1
				}
				encodedID := values[2][item]
				var inGroupID uint64
				switch {
				case previous == -1:
					inGroupID = uint64(encodedID)
				case encodedID == 0:
					inGroupID = 0
				case encodedID == 1:
					inGroupID = uint64(previous + 1)
				default:
					inGroupID = zigzagDecodeRelative(uint64(encodedID-1), uint64(previous+1))
				}
				rawLength := zigzagDecodeRelative(uint64(values[3][item]), predictedLength)
				if inGroupID > uint64(^uint32(0)) || rawLength > uint64(^uint32(0)) || values[4][item] > 1 {
					return fmt.Errorf("%w: invalid segment descriptor", ErrCorruptArchive)
				}
				details[i][j][k] = segmentDescriptor{
					groupID: groupID, inGroupID: uint32(inGroupID),
					reversed: values[4][item] != 0, rawLength: uint32(rawLength),
				}
				if int64(inGroupID) > previous && inGroupID > 0 {
					previousIDs[groupID] = int64(inGroupID)
				}
				item++
			}
		}
	}
	a.contigDetails = details
	a.detailsBatchID = batchID
	return nil
}

func zigzagDecodeRelative(value, previous uint64) uint64 {
	if value >= 2*previous {
		return value
	}
	if value&1 != 0 {
		return (2*previous - value) / 2
	}
	return (value + 2*previous) / 2
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
	decoder, err := newBoundedZstdDecoder(maxSize)
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

func newBoundedZstdDecoder(maxSize uint64) (*zstd.Decoder, error) {
	return zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(maxSize),
		zstd.WithDecodeAllCapLimit(true),
	)
}
