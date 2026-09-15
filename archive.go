// Package agc reads Assembled Genomes Compressor (AGC) v3 archives.
//
// Archives are decoded lazily from an io.ReaderAt. The package is read-only.
package agc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
)

const maxFileTypeInfoSize = 16 << 20

var (
	// ErrClosed is returned when an operation uses a closed archive.
	ErrClosed = errors.New("agc: archive is closed")
	// ErrCorruptArchive marks malformed, truncated, or inconsistent input.
	ErrCorruptArchive = errors.New("agc: corrupt archive")
	// ErrUnsupportedVersion marks an AGC archive whose file-format major version is not 3.
	ErrUnsupportedVersion = errors.New("agc: unsupported file-format version")
	// ErrSampleNotFound marks a lookup for a sample not present in the archive.
	ErrSampleNotFound = errors.New("agc: sample not found")
	// ErrContigNotFound marks a lookup for a contig not present in the selected sample.
	ErrContigNotFound = errors.New("agc: contig not found")
)

// Version is the AGC file-format version, not the producer's software version.
type Version struct {
	Major uint32
	Minor uint32
}

// Sample identifies a sample independently from any of its contigs.
type Sample struct {
	Name string
}

// ContigInfo identifies a contig and its owning sample separately.
type ContigInfo struct {
	Sample Sample
	Name   string
}

// Contig contains one decoded contig.
type Contig struct {
	Sample   Sample
	Name     string
	Sequence []byte
}

// Archive is an open, read-only AGC v3 archive.
type Archive struct {
	mu                  sync.Mutex
	r                   io.ReaderAt
	size                int64
	index               archiveIndex
	version             Version
	closer              io.Closer
	closed              bool
	samples             []Sample
	sampleIDs           map[string]int
	batchSize           uint32
	kmerLength          uint32
	minMatchLen         uint32
	segmentSize         uint32
	paramsLoaded        bool
	contigBatchID       int
	contigBatch         [][]string
	detailsBatchID      int
	contigDetails       [][][]segmentDescriptor
	namedContigSampleID int
	namedContigIDs      map[string]int
	referenceCache      *boundedByteCache[uint32]
	packCache           *boundedByteCache[segmentPackKey]
}

// Open opens a local AGC v3 archive. Close releases the underlying file.
func Open(path string) (*Archive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("agc: open %q: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("agc: stat %q: %w", path, err)
	}
	a, err := openReaderAt(f, info.Size(), f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("agc: open %q: %w", path, err)
	}
	return a, nil
}

// OpenReaderAt opens an AGC v3 archive from a ReaderAt and its exact size.
// The caller retains ownership of r; Archive.Close does not close it.
func OpenReaderAt(r io.ReaderAt, size int64) (*Archive, error) {
	return openReaderAt(r, size, nil)
}

func openReaderAt(r io.ReaderAt, size int64, closer io.Closer) (*Archive, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil ReaderAt", ErrCorruptArchive)
	}
	index, err := readArchiveIndex(r, size)
	if err != nil {
		return nil, err
	}
	a := &Archive{
		r: r, size: size, index: index, closer: closer,
		contigBatchID: -1, detailsBatchID: -1, namedContigSampleID: -1,
		referenceCache: newBoundedByteCache[uint32](maxSegmentCacheBytes),
		packCache:      newBoundedByteCache[segmentPackKey](maxSegmentCacheBytes),
	}
	version, err := a.readVersion()
	if err != nil {
		return nil, err
	}
	if version.Major != 3 {
		return nil, fmt.Errorf("%w: got %d.%d, require major 3", ErrUnsupportedVersion, version.Major, version.Minor)
	}
	if _, ok := index.streams["collection-samples"]; !ok {
		return nil, fmt.Errorf("%w: missing collection-samples stream", ErrCorruptArchive)
	}
	a.version = version
	return a, nil
}

// Close closes the archive. It is safe to call more than once.
func (a *Archive) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	a.r = nil
	if a.closer != nil {
		return a.closer.Close()
	}
	return nil
}

// Version returns the validated AGC file-format version.
func (a *Archive) Version() Version { return a.version }

func (a *Archive) readVersion() (Version, error) {
	part, err := a.index.part("file_type_info", 0)
	if err != nil {
		return Version{}, err
	}
	data, itemCount, err := readPart(a.r, a.index.dataEnd, part, maxFileTypeInfoSize)
	if err != nil {
		return Version{}, fmt.Errorf("file_type_info: %w", err)
	}
	if itemCount > uint64(len(data)/2) {
		return Version{}, fmt.Errorf("%w: impossible file_type_info item count %d", ErrCorruptArchive, itemCount)
	}
	c := byteCursor{data: data}
	values := make(map[string]string, itemCount)
	for i := uint64(0); i < itemCount; i++ {
		key, err := c.cstring()
		if err != nil {
			return Version{}, fmt.Errorf("file_type_info key %d: %w", i, err)
		}
		value, err := c.cstring()
		if err != nil {
			return Version{}, fmt.Errorf("file_type_info value %d: %w", i, err)
		}
		if _, exists := values[key]; exists {
			return Version{}, fmt.Errorf("%w: duplicate file_type_info key %q", ErrCorruptArchive, key)
		}
		values[key] = value
	}
	if !c.empty() {
		return Version{}, fmt.Errorf("%w: trailing file_type_info data", ErrCorruptArchive)
	}
	major, err := parseVersionField(values, "file_version_major")
	if err != nil {
		return Version{}, err
	}
	minor, err := parseVersionField(values, "file_version_minor")
	if err != nil {
		return Version{}, err
	}
	return Version{Major: major, Minor: minor}, nil
}

func parseVersionField(values map[string]string, key string) (uint32, error) {
	value, ok := values[key]
	if !ok {
		return 0, fmt.Errorf("%w: missing %s", ErrCorruptArchive, key)
	}
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid %s %q", ErrCorruptArchive, key, value)
	}
	return uint32(n), nil
}
