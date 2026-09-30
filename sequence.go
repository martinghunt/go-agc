package agc

// Sequence decoding in this file is derived from REFRESH Bioinformatics
// Group's AGC segment.cpp, lz_diff.cpp, and agc_decompressor_lib.cpp,
// distributed under the MIT license. See THIRD_PARTY_NOTICES.md and
// third_party/agc.LICENSE.

import (
	"bytes"
	"fmt"
	"math"
	"strings"
)

const (
	rawSegmentGroups     = 16
	maxSequenceBlockSize = 1 << 30
	segmentSeparator     = 0xff
	// maxSegmentCacheBytes bounds each of the archive's reference and delta
	// pack caches, so repeated Contig calls reuse already-decompressed
	// segment data (the common case: many contigs share a small set of
	// reference groups) without retaining the whole archive in memory.
	maxSegmentCacheBytes = 256 << 20
)

// boundedCache holds decoded segment data behind a byte budget, evicting the
// oldest entries first once the budget is exceeded. size is the caller's
// accounting cost for value (e.g. the underlying backing array's length),
// since a cached value need not be a []byte itself.
type boundedCache[K comparable, V any] struct {
	maxBytes int
	bytes    int
	order    []K
	entries  map[K]cacheEntry[V]
}

type cacheEntry[V any] struct {
	value V
	size  int
}

func newBoundedCache[K comparable, V any](maxBytes int) *boundedCache[K, V] {
	return &boundedCache[K, V]{maxBytes: maxBytes, entries: make(map[K]cacheEntry[V])}
}

func (c *boundedCache[K, V]) get(key K) (V, bool) {
	entry, ok := c.entries[key]
	return entry.value, ok
}

func (c *boundedCache[K, V]) put(key K, value V, size int) {
	if _, exists := c.entries[key]; exists {
		return
	}
	c.entries[key] = cacheEntry[V]{value: value, size: size}
	c.order = append(c.order, key)
	c.bytes += size
	for c.bytes > c.maxBytes && len(c.order) > 1 {
		oldest := c.order[0]
		c.order = c.order[1:]
		c.bytes -= c.entries[oldest].size
		delete(c.entries, oldest)
	}
}

// Contig retrieves and decodes a named contig from a sample. Name matching
// follows AGC: the query and stored header are compared up to the first ASCII
// whitespace, while the returned Contig.Name retains the full stored header.
func (a *Archive) Contig(sample Sample, name string) (Contig, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return Contig{}, ErrClosed
	}
	if a.samples == nil {
		if err := a.loadSamples(); err != nil {
			return Contig{}, err
		}
	}
	sampleID, ok := a.sampleIDs[sample.Name]
	if !ok {
		return Contig{}, fmt.Errorf("%w: %q", ErrSampleNotFound, sample.Name)
	}
	if err := a.loadParams(); err != nil {
		return Contig{}, err
	}
	batchID := sampleID / int(a.batchSize)
	if a.contigBatchID != batchID {
		if err := a.loadContigBatch(batchID); err != nil {
			return Contig{}, err
		}
	}
	if err := a.loadContigDetails(batchID); err != nil {
		return Contig{}, err
	}
	withinBatch := sampleID - batchID*int(a.batchSize)
	contigID := a.namedContigID(sampleID, a.contigBatch[withinBatch], name)
	if contigID < 0 {
		return Contig{}, fmt.Errorf("%w: %q in sample %q", ErrContigNotFound, name, sample.Name)
	}
	if err := a.prefetchDescriptorsLocked(a.contigDetails[withinBatch][contigID]); err != nil {
		return Contig{}, err
	}
	return a.decodeContigLocked(sampleID, withinBatch, contigID)
}

func (a *Archive) namedContigID(sampleID int, names []string, name string) int {
	if a.namedContigSampleID != sampleID {
		ids := make(map[string]int, len(names))
		for i, stored := range names {
			short := shortContigName(stored)
			if _, exists := ids[short]; !exists {
				ids[short] = i
			}
		}
		a.namedContigSampleID = sampleID
		a.namedContigIDs = ids
	}
	id, ok := a.namedContigIDs[shortContigName(name)]
	if !ok {
		return -1
	}
	return id
}

func (a *Archive) decodeContigLocked(sampleID, withinBatch, contigID int) (Contig, error) {
	decoder := segmentDecoder{archive: a}
	descriptors := a.contigDetails[withinBatch][contigID]
	var numeric []byte
	for i, descriptor := range descriptors {
		segment, err := decoder.decode(descriptor)
		if err != nil {
			return Contig{}, fmt.Errorf("decode %s segment %d: %w", a.contigBatch[withinBatch][contigID], i, err)
		}
		if descriptor.reversed {
			reverseComplement(segment)
		}
		if i == 0 {
			numeric = append(numeric, segment...)
			continue
		}
		if len(segment) < int(a.kmerLength) {
			return Contig{}, fmt.Errorf("%w: segment shorter than k-mer overlap", ErrCorruptArchive)
		}
		numeric = append(numeric, segment[a.kmerLength:]...)
	}
	sequence, err := nucleotideText(numeric)
	if err != nil {
		return Contig{}, err
	}
	return Contig{Sample: a.samples[sampleID], Name: a.contigBatch[withinBatch][contigID], Sequence: sequence}, nil
}

func (a *Archive) prefetchSample(sample Sample) error {
	prefetcher, ok := a.r.(rangePrefetcher)
	if !ok {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrClosed
	}
	if a.samples == nil {
		if err := a.loadSamples(); err != nil {
			return err
		}
	}
	sampleID, ok := a.sampleIDs[sample.Name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrSampleNotFound, sample.Name)
	}
	if err := a.loadParams(); err != nil {
		return err
	}
	batchID := sampleID / int(a.batchSize)
	if a.contigBatchID != batchID {
		if err := a.loadContigBatch(batchID); err != nil {
			return err
		}
	}
	if err := a.loadContigDetails(batchID); err != nil {
		return err
	}
	withinBatch := sampleID - batchID*int(a.batchSize)
	var descriptors []segmentDescriptor
	for _, contig := range a.contigDetails[withinBatch] {
		descriptors = append(descriptors, contig...)
	}
	return a.prefetchDescriptorsWithLocked(prefetcher, descriptors)
}

func (a *Archive) prefetchDescriptorsLocked(descriptors []segmentDescriptor) error {
	prefetcher, ok := a.r.(rangePrefetcher)
	if !ok {
		return nil
	}
	return a.prefetchDescriptorsWithLocked(prefetcher, descriptors)
}

func (a *Archive) prefetchDescriptorsWithLocked(prefetcher rangePrefetcher, descriptors []segmentDescriptor) error {
	ranges := make(map[readRange]struct{}, len(descriptors)*2)
	addPart := func(stream string, partID int) error {
		part, err := a.index.part(stream, partID)
		if err != nil {
			return err
		}
		if part.size == 0 {
			return nil
		}
		available := a.index.dataEnd - part.offset
		length := part.size
		if available-length > 9 {
			length += 9
		} else {
			length = available
		}
		ranges[readRange{offset: int64(part.offset), length: int64(length)}] = struct{}{}
		return nil
	}
	for _, descriptor := range descriptors {
		if descriptor.groupID < rawSegmentGroups {
			partID := int(descriptor.inGroupID / a.batchSize)
			if err := addPart(segmentStreamName(descriptor.groupID, 'd'), partID); err != nil {
				return err
			}
			continue
		}
		if err := addPart(segmentStreamName(descriptor.groupID, 'r'), 0); err != nil {
			return err
		}
		if descriptor.inGroupID != 0 {
			partID := int((descriptor.inGroupID - 1) / a.batchSize)
			if err := addPart(segmentStreamName(descriptor.groupID, 'd'), partID); err != nil {
				return err
			}
		}
	}
	requests := make([]readRange, 0, len(ranges))
	for requestRange := range ranges {
		requests = append(requests, requestRange)
	}
	if err := prefetcher.Prefetch(requests); err != nil {
		return fmt.Errorf("agc: prefetch remote sequence data: %w", err)
	}
	return nil
}

func (a *Archive) contigAt(sample Sample, contigID int) (Contig, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return Contig{}, ErrClosed
	}
	if a.samples == nil {
		if err := a.loadSamples(); err != nil {
			return Contig{}, err
		}
	}
	sampleID, ok := a.sampleIDs[sample.Name]
	if !ok {
		return Contig{}, fmt.Errorf("%w: %q", ErrSampleNotFound, sample.Name)
	}
	if err := a.loadParams(); err != nil {
		return Contig{}, err
	}
	batchID := sampleID / int(a.batchSize)
	if a.contigBatchID != batchID {
		if err := a.loadContigBatch(batchID); err != nil {
			return Contig{}, err
		}
	}
	if err := a.loadContigDetails(batchID); err != nil {
		return Contig{}, err
	}
	withinBatch := sampleID - batchID*int(a.batchSize)
	if contigID < 0 || contigID >= len(a.contigBatch[withinBatch]) {
		return Contig{}, fmt.Errorf("%w: contig index %d in sample %q", ErrCorruptArchive, contigID, sample.Name)
	}
	return a.decodeContigLocked(sampleID, withinBatch, contigID)
}

func shortContigName(name string) string {
	if i := strings.IndexAny(name, " \n\r\t"); i >= 0 {
		return name[:i]
	}
	return name
}

type segmentPackKey struct {
	group uint32
	part  int
}

type segmentDecoder struct {
	archive *Archive
}

func (d *segmentDecoder) decode(descriptor segmentDescriptor) ([]byte, error) {
	var sequence []byte
	var err error
	if descriptor.groupID < rawSegmentGroups {
		sequence, err = d.decodeRaw(descriptor.groupID, descriptor.inGroupID)
	} else {
		sequence, err = d.decodeDelta(descriptor.groupID, descriptor.inGroupID, descriptor.rawLength)
	}
	if err != nil {
		return nil, err
	}
	if len(sequence) != int(descriptor.rawLength) {
		return nil, fmt.Errorf("%w: segment length %d, want %d", ErrCorruptArchive, len(sequence), descriptor.rawLength)
	}
	return sequence, nil
}

func (d *segmentDecoder) decodeRaw(groupID, sequenceID uint32) ([]byte, error) {
	packSize := d.archive.batchSize
	partID := int(sequenceID / packSize)
	parts, err := d.loadPack(groupID, partID)
	if err != nil {
		return nil, err
	}
	return sequenceAt(parts, int(sequenceID%packSize))
}

func (d *segmentDecoder) decodeDelta(groupID, sequenceID, expectedSize uint32) ([]byte, error) {
	reference, err := d.reference(groupID)
	if err != nil {
		return nil, err
	}
	if sequenceID == 0 {
		return append([]byte(nil), reference...), nil
	}
	packSize := d.archive.batchSize
	partID := int((sequenceID - 1) / packSize)
	parts, err := d.loadPack(groupID, partID)
	if err != nil {
		return nil, err
	}
	delta, err := sequenceAt(parts, int((sequenceID-1)%packSize))
	if err != nil {
		return nil, err
	}
	return decodeLZ(reference, delta, d.archive.minMatchLen, expectedSize)
}

// loadPack returns a delta-pack's sequences, split once and cached, rather
// than rescanning the whole pack from byte zero on every lookup.
func (d *segmentDecoder) loadPack(groupID uint32, partID int) ([][]byte, error) {
	key := segmentPackKey{group: groupID, part: partID}
	if parts, ok := d.archive.packCache.get(key); ok {
		return parts, nil
	}
	raw, err := d.readPackedStream(segmentStreamName(groupID, 'd'), partID)
	if err != nil {
		return nil, err
	}
	parts := splitSegmentPack(raw)
	d.archive.packCache.put(key, parts, len(raw))
	return parts, nil
}

func (d *segmentDecoder) reference(groupID uint32) ([]byte, error) {
	if reference, ok := d.archive.referenceCache.get(groupID); ok {
		return reference, nil
	}
	part, err := d.archive.index.part(segmentStreamName(groupID, 'r'), 0)
	if err != nil {
		return nil, err
	}
	packed, rawSize, err := readPart(d.archive.r, d.archive.index.dataEnd, part, maxSequenceBlockSize)
	if err != nil {
		return nil, err
	}
	var reference []byte
	if rawSize == 0 {
		reference = packed
	} else {
		if len(packed) < 2 || rawSize > maxSequenceBlockSize {
			return nil, fmt.Errorf("%w: invalid packed reference", ErrCorruptArchive)
		}
		marker := packed[len(packed)-1]
		decoded, err := decodeZstdAtMost(packed[:len(packed)-1], rawSize+1, maxSequenceBlockSize)
		if err != nil {
			return nil, err
		}
		switch marker {
		case 0:
			reference = decoded
		case 1:
			reference, err = tuplesToBytes(decoded, rawSize)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("%w: unknown reference packing marker %d", ErrCorruptArchive, marker)
		}
		if uint64(len(reference)) != rawSize {
			return nil, fmt.Errorf("%w: reference length %d, want %d", ErrCorruptArchive, len(reference), rawSize)
		}
	}
	d.archive.referenceCache.put(groupID, reference, len(reference))
	return reference, nil
}

func (d *segmentDecoder) readPackedStream(name string, partID int) ([]byte, error) {
	part, err := d.archive.index.part(name, partID)
	if err != nil {
		return nil, err
	}
	packed, rawSize, err := readPart(d.archive.r, d.archive.index.dataEnd, part, maxSequenceBlockSize)
	if err != nil {
		return nil, err
	}
	if rawSize == 0 {
		return packed, nil
	}
	if len(packed) < 2 || packed[len(packed)-1] != 0 {
		return nil, fmt.Errorf("%w: invalid compressed segment marker", ErrCorruptArchive)
	}
	return decodeZstdBlock(packed[:len(packed)-1], rawSize, maxSequenceBlockSize)
}

// splitSegmentPack splits a segment pack into its separator-terminated
// sequences once, so repeated lookups by index don't rescan the pack from
// the start every time. bytes.Split's final element is whatever follows the
// last separator (empty when the pack is properly terminated, a truncated
// leftover otherwise); dropping it matches the wire format, where a sequence
// only exists once its terminating separator has been seen.
func splitSegmentPack(pack []byte) [][]byte {
	parts := bytes.Split(pack, []byte{segmentSeparator})
	return parts[:len(parts)-1]
}

func sequenceAt(parts [][]byte, index int) ([]byte, error) {
	if index < 0 || index >= len(parts) {
		return nil, fmt.Errorf("%w: sequence %d missing from segment pack", ErrCorruptArchive, index)
	}
	return append([]byte(nil), parts[index]...), nil
}

func segmentStreamName(groupID uint32, suffix byte) string {
	const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_#"
	name := []byte{'x'}
	for {
		name = append(name, digits[groupID&0x3f])
		groupID /= 64
		if groupID == 0 {
			break
		}
	}
	return string(append(name, suffix))
}

func decodeLZ(reference, encoded []byte, minMatchLen, expectedSize uint32) ([]byte, error) {
	decoded := make([]byte, 0, expectedSize)
	predicted := int64(0)
	for pos := 0; pos < len(encoded); {
		b := encoded[pos]
		switch {
		case (b >= 'A' && b <= 'U') || b == '!':
			pos++
			var value byte
			if b == '!' {
				if predicted < 0 || predicted >= int64(len(reference)) {
					return nil, fmt.Errorf("%w: literal reference position out of bounds", ErrCorruptArchive)
				}
				value = reference[predicted]
			} else {
				value = b - 'A'
			}
			decoded = append(decoded, value)
			predicted++
		case b == 30:
			pos++
			rawLength, next, err := readDecimal(encoded, pos)
			if err != nil || rawLength < 0 || next >= len(encoded) || encoded[next] != 4 {
				return nil, fmt.Errorf("%w: invalid N run", ErrCorruptArchive)
			}
			pos = next + 1
			length := rawLength + 4
			if length > int64(expectedSize)-int64(len(decoded)) {
				return nil, fmt.Errorf("%w: N run exceeds segment size", ErrCorruptArchive)
			}
			decoded = append(decoded, make([]byte, int(length))...)
			for i := len(decoded) - int(length); i < len(decoded); i++ {
				decoded[i] = 4
			}
		default:
			delta, next, err := readDecimal(encoded, pos)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid match position", ErrCorruptArchive)
			}
			refPos := predicted + delta
			pos = next
			var length int64
			if pos < len(encoded) && encoded[pos] == ',' {
				lengthDelta, afterLength, err := readDecimal(encoded, pos+1)
				if err != nil || lengthDelta < 0 {
					return nil, fmt.Errorf("%w: invalid match length", ErrCorruptArchive)
				}
				length = lengthDelta + int64(minMatchLen)
				pos = afterLength
			} else {
				length = int64(len(reference)) - refPos
			}
			if pos >= len(encoded) || encoded[pos] != '.' {
				return nil, fmt.Errorf("%w: unterminated match", ErrCorruptArchive)
			}
			pos++
			if refPos < 0 || length < 0 || refPos+length > int64(len(reference)) || length > int64(expectedSize)-int64(len(decoded)) {
				return nil, fmt.Errorf("%w: match exceeds reference or segment", ErrCorruptArchive)
			}
			decoded = append(decoded, reference[refPos:refPos+length]...)
			predicted = refPos + length
		}
		if len(decoded) > int(expectedSize) {
			return nil, fmt.Errorf("%w: decoded segment exceeds expected size", ErrCorruptArchive)
		}
	}
	return decoded, nil
}

func readDecimal(data []byte, pos int) (int64, int, error) {
	negative := false
	if pos < len(data) && data[pos] == '-' {
		negative = true
		pos++
	}
	start := pos
	var value int64
	for pos < len(data) && data[pos] >= '0' && data[pos] <= '9' {
		digit := int64(data[pos] - '0')
		if value > (math.MaxInt64-digit)/10 {
			return 0, pos, fmt.Errorf("integer overflow")
		}
		value = value*10 + digit
		pos++
	}
	if pos == start {
		return 0, pos, fmt.Errorf("missing integer")
	}
	if negative {
		value = -value
	}
	return value, pos, nil
}

func tuplesToBytes(tuples []byte, expected uint64) ([]byte, error) {
	if len(tuples) < 2 || expected > maxSequenceBlockSize {
		return nil, fmt.Errorf("%w: invalid tuple-packed reference", ErrCorruptArchive)
	}
	marker := tuples[len(tuples)-1]
	width := int(marker >> 4)
	trailing := int(marker & 0x0f)
	if width == 1 {
		out := append([]byte(nil), tuples[:len(tuples)-1]...)
		if uint64(len(out)) != expected {
			return nil, fmt.Errorf("%w: tuple output length mismatch", ErrCorruptArchive)
		}
		return out, nil
	}
	multiplier := 0
	switch width {
	case 2:
		multiplier = 16
	case 3:
		multiplier = 6
	case 4:
		multiplier = 4
	default:
		return nil, fmt.Errorf("%w: invalid tuple width %d", ErrCorruptArchive, width)
	}
	if trailing < 0 || trailing >= width {
		return nil, fmt.Errorf("%w: invalid tuple trailing count", ErrCorruptArchive)
	}
	out := make([]byte, 0, expected)
	values := tuples[:len(tuples)-1]
	for i, value := range values {
		n := width
		if i == len(values)-1 {
			n = trailing
		}
		var decoded [4]byte
		v := int(value)
		for j := n - 1; j >= 0; j-- {
			decoded[j] = byte(v % multiplier)
			v /= multiplier
		}
		out = append(out, decoded[:n]...)
	}
	if uint64(len(out)) != expected {
		return nil, fmt.Errorf("%w: tuple output length %d, want %d", ErrCorruptArchive, len(out), expected)
	}
	return out, nil
}

func decodeZstdAtMost(packed []byte, capacity, maxSize uint64) ([]byte, error) {
	if capacity == 0 || capacity > maxSize || capacity > uint64(math.MaxInt) {
		return nil, fmt.Errorf("%w: invalid zstd output bound %d", ErrCorruptArchive, capacity)
	}
	decoder, err := newBoundedZstdDecoder(maxSize)
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(packed, make([]byte, 0, int(capacity)))
	if err != nil {
		return nil, fmt.Errorf("%w: decompress zstd block: %v", ErrCorruptArchive, err)
	}
	return decoded, nil
}

func reverseComplement(sequence []byte) {
	for left, right := 0, len(sequence)-1; left <= right; left, right = left+1, right-1 {
		a, b := sequence[left], sequence[right]
		if a < 4 {
			a = 3 - a
		}
		if b < 4 {
			b = 3 - b
		}
		sequence[left], sequence[right] = b, a
	}
}

func nucleotideText(numeric []byte) ([]byte, error) {
	const alphabet = "ACGTNRYSWKMBDHVU"
	text := make([]byte, len(numeric))
	for i, value := range numeric {
		if int(value) >= len(alphabet) {
			return nil, fmt.Errorf("%w: invalid nucleotide code %d", ErrCorruptArchive, value)
		}
		text[i] = alphabet[value]
	}
	return text, nil
}
