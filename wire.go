package agc

// Wire encodings in this file are derived from REFRESH Bioinformatics Group's
// AGC archive.cpp and collection.h, distributed under the MIT license. See
// THIRD_PARTY_NOTICES.md and third_party/agc.LICENSE.

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

const (
	maxFooterSize   = 256 << 20
	maxIndexStreams = 1 << 20
	maxIndexParts   = 1 << 20
)

type archivePart struct {
	offset uint64
	size   uint64
}

type archiveStream struct {
	parts []archivePart
}

type archiveIndex struct {
	streams map[string]archiveStream
	dataEnd uint64
}

func readArchiveIndex(r io.ReaderAt, size int64) (archiveIndex, error) {
	if size < 8 {
		return archiveIndex{}, fmt.Errorf("%w: file is %d bytes; footer length requires 8", ErrCorruptArchive, size)
	}
	var tail [8]byte
	if err := readAtFull(r, tail[:], size-8); err != nil {
		return archiveIndex{}, fmt.Errorf("%w: read footer length: %v", ErrCorruptArchive, err)
	}
	footerSize := binary.LittleEndian.Uint64(tail[:])
	if footerSize == 0 || footerSize > uint64(size-8) || footerSize > maxFooterSize || footerSize > uint64(math.MaxInt) {
		return archiveIndex{}, fmt.Errorf("%w: invalid footer length %d for file size %d", ErrCorruptArchive, footerSize, size)
	}
	footerStart := uint64(size-8) - footerSize
	footer := make([]byte, int(footerSize))
	if err := readAtFull(r, footer, int64(footerStart)); err != nil {
		return archiveIndex{}, fmt.Errorf("%w: read footer: %v", ErrCorruptArchive, err)
	}
	c := byteCursor{data: footer}
	streamCount, err := c.archiveUint()
	if err != nil || streamCount > maxIndexStreams {
		return archiveIndex{}, fmt.Errorf("%w: invalid stream count", ErrCorruptArchive)
	}
	index := archiveIndex{streams: make(map[string]archiveStream, streamCount), dataEnd: footerStart}
	var totalParts uint64
	for i := uint64(0); i < streamCount; i++ {
		name, err := c.cstring()
		if err != nil || name == "" {
			return archiveIndex{}, fmt.Errorf("%w: invalid stream %d name", ErrCorruptArchive, i)
		}
		if _, exists := index.streams[name]; exists {
			return archiveIndex{}, fmt.Errorf("%w: duplicate stream %q", ErrCorruptArchive, name)
		}
		partCount, err := c.archiveUint()
		totalParts += partCount
		if err != nil || partCount > uint64(c.remaining()/2) || totalParts > maxIndexParts {
			return archiveIndex{}, fmt.Errorf("%w: invalid part count for stream %q", ErrCorruptArchive, name)
		}
		if _, err := c.archiveUint(); err != nil { // aggregate raw size, unused by the reader
			return archiveIndex{}, fmt.Errorf("%w: invalid raw size for stream %q", ErrCorruptArchive, name)
		}
		stream := archiveStream{parts: make([]archivePart, 0, int(partCount))}
		for j := uint64(0); j < partCount; j++ {
			offset, err := c.archiveUint()
			if err != nil {
				return archiveIndex{}, fmt.Errorf("%w: invalid offset for %q part %d", ErrCorruptArchive, name, j)
			}
			partSize, err := c.archiveUint()
			if err != nil || offset > footerStart || partSize > footerStart-offset || (partSize != 0 && partSize+1 > footerStart-offset) {
				return archiveIndex{}, fmt.Errorf("%w: invalid bounds for %q part %d", ErrCorruptArchive, name, j)
			}
			stream.parts = append(stream.parts, archivePart{offset: offset, size: partSize})
		}
		index.streams[name] = stream
	}
	if !c.empty() {
		return archiveIndex{}, fmt.Errorf("%w: trailing footer data", ErrCorruptArchive)
	}
	return index, nil
}

func (i archiveIndex) part(streamName string, partNumber int) (archivePart, error) {
	stream, ok := i.streams[streamName]
	if !ok {
		return archivePart{}, fmt.Errorf("%w: missing %s stream", ErrCorruptArchive, streamName)
	}
	if partNumber < 0 || partNumber >= len(stream.parts) {
		return archivePart{}, fmt.Errorf("%w: missing %s part %d", ErrCorruptArchive, streamName, partNumber)
	}
	return stream.parts[partNumber], nil
}

func readPart(r io.ReaderAt, dataEnd uint64, part archivePart, maxSize uint64) ([]byte, uint64, error) {
	if part.size == 0 {
		return nil, 0, nil
	}
	var prefix [1]byte
	if err := readAtFull(r, prefix[:], int64(part.offset)); err != nil {
		return nil, 0, fmt.Errorf("%w: read part metadata: %v", ErrCorruptArchive, err)
	}
	n := int(prefix[0])
	if n > 8 || part.offset+uint64(1+n) > dataEnd || part.size > dataEnd-part.offset-uint64(1+n) || part.size > maxSize || part.size > uint64(math.MaxInt) {
		return nil, 0, fmt.Errorf("%w: invalid part metadata or size", ErrCorruptArchive)
	}
	metadataBytes := make([]byte, n)
	if err := readAtFull(r, metadataBytes, int64(part.offset+1)); err != nil {
		return nil, 0, fmt.Errorf("%w: read part metadata: %v", ErrCorruptArchive, err)
	}
	var metadata uint64
	for _, b := range metadataBytes {
		metadata = metadata<<8 | uint64(b)
	}
	data := make([]byte, int(part.size))
	if err := readAtFull(r, data, int64(part.offset+1+uint64(n))); err != nil {
		return nil, 0, fmt.Errorf("%w: read part data: %v", ErrCorruptArchive, err)
	}
	return data, metadata, nil
}

func readAtFull(r io.ReaderAt, p []byte, off int64) error {
	if len(p) == 0 {
		return nil
	}
	n, err := r.ReadAt(p, off)
	if n != len(p) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}

type byteCursor struct {
	data []byte
	pos  int
}

func (c *byteCursor) remaining() int { return len(c.data) - c.pos }
func (c *byteCursor) empty() bool    { return c.pos == len(c.data) }

func (c *byteCursor) archiveUint() (uint64, error) {
	if c.remaining() < 1 {
		return 0, fmt.Errorf("%w: truncated integer", ErrCorruptArchive)
	}
	n := int(c.data[c.pos])
	c.pos++
	if n > 8 || c.remaining() < n {
		return 0, fmt.Errorf("%w: invalid integer length %d", ErrCorruptArchive, n)
	}
	var value uint64
	for _, b := range c.data[c.pos : c.pos+n] {
		value = value<<8 | uint64(b)
	}
	c.pos += n
	return value, nil
}

func (c *byteCursor) cstring() (string, error) {
	start := c.pos
	for c.pos < len(c.data) && c.data[c.pos] != 0 {
		c.pos++
	}
	if c.pos == len(c.data) {
		return "", fmt.Errorf("%w: unterminated string", ErrCorruptArchive)
	}
	value := string(c.data[start:c.pos])
	c.pos++
	return value, nil
}

func (c *byteCursor) collectionUint() (uint32, error) {
	if c.remaining() < 1 {
		return 0, fmt.Errorf("%w: truncated collection integer", ErrCorruptArchive)
	}
	first := c.data[c.pos]
	length := [...]int{1, 1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 4, 5}[first>>4]
	if c.remaining() < length {
		return 0, fmt.Errorf("%w: truncated collection integer", ErrCorruptArchive)
	}
	var value uint32
	switch length {
	case 1:
		value = uint32(first)
	case 2:
		value = uint32(first&0x3f)<<8 | uint32(c.data[c.pos+1])
		value += 1 << 7
	case 3:
		value = uint32(first&0x1f)<<16 | uint32(c.data[c.pos+1])<<8 | uint32(c.data[c.pos+2])
		value += (1 << 7) + (1 << 14)
	case 4:
		value = uint32(first&0x0f)<<24 | uint32(c.data[c.pos+1])<<16 | uint32(c.data[c.pos+2])<<8 | uint32(c.data[c.pos+3])
		value += (1 << 7) + (1 << 14) + (1 << 21)
	case 5:
		value = binary.BigEndian.Uint32(c.data[c.pos+1 : c.pos+5])
		value += (1 << 7) + (1 << 14) + (1 << 21) + (1 << 28)
	}
	c.pos += length
	return value, nil
}
