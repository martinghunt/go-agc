package agc

import (
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"
)

func TestClose_GivenOwnedCloserError_WhenClosingTwice_ThenReturnsItOnlyOnce(t *testing.T) {
	want := errors.New("close failed")
	a := &Archive{closer: errorCloser{err: want}}
	if err := a.Close(); !errors.Is(err, want) {
		t.Fatalf("first Close() error = %v, want %v", err, want)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestByteCursor_GivenEveryCollectionIntegerWidth_WhenRead_ThenDecodesThresholds(t *testing.T) {
	tests := []struct {
		encoded []byte
		want    uint32
	}{
		{[]byte{0x7f}, 127},
		{[]byte{0x80, 0}, 1 << 7},
		{[]byte{0xc0, 0, 0}, (1 << 7) + (1 << 14)},
		{[]byte{0xe0, 0, 0, 0}, (1 << 7) + (1 << 14) + (1 << 21)},
		{[]byte{0xf0, 0, 0, 0, 0}, (1 << 7) + (1 << 14) + (1 << 21) + (1 << 28)},
	}
	for _, tt := range tests {
		cursor := byteCursor{data: tt.encoded}
		got, err := cursor.collectionUint()
		if err != nil {
			t.Fatalf("collectionUint(%x): %v", tt.encoded, err)
		}
		if got != tt.want || !cursor.empty() {
			t.Errorf("collectionUint(%x) = %d, want %d", tt.encoded, got, tt.want)
		}
	}
}

func TestByteCursor_GivenMalformedWireValues_WhenRead_ThenRejectsThem(t *testing.T) {
	for _, encoded := range [][]byte{nil, {0x80}, {0xc0, 0}, {0xe0, 0, 0}, {0xf0, 0, 0, 0}} {
		cursor := byteCursor{data: encoded}
		if _, err := cursor.collectionUint(); !errors.Is(err, ErrCorruptArchive) {
			t.Errorf("collectionUint(%x) error = %v", encoded, err)
		}
	}
	for _, encoded := range [][]byte{nil, {9}, {2, 1}} {
		cursor := byteCursor{data: encoded}
		if _, err := cursor.archiveUint(); !errors.Is(err, ErrCorruptArchive) {
			t.Errorf("archiveUint(%x) error = %v", encoded, err)
		}
	}
	cursor := byteCursor{data: []byte("unterminated")}
	if _, err := cursor.cstring(); !errors.Is(err, ErrCorruptArchive) {
		t.Errorf("cstring() error = %v", err)
	}
}

func TestReadPart_GivenEmptyValidAndMalformedParts_WhenRead_ThenChecksBounds(t *testing.T) {
	if data, metadata, err := readPart(bytes.NewReader(nil), 0, archivePart{}, 10); err != nil || data != nil || metadata != 0 {
		t.Fatalf("empty readPart() = %v, %d, %v", data, metadata, err)
	}
	wire := appendTestArchiveUint(nil, 513)
	wire = append(wire, 7, 8)
	data, metadata, err := readPart(bytes.NewReader(wire), uint64(len(wire)), archivePart{size: 2}, 2)
	if err != nil || metadata != 513 || !reflect.DeepEqual(data, []byte{7, 8}) {
		t.Fatalf("readPart() = %v, %d, %v", data, metadata, err)
	}

	tests := []struct {
		wire []byte
		part archivePart
		max  uint64
	}{
		{[]byte{9}, archivePart{size: 1}, 1},
		{[]byte{1, 0, 7}, archivePart{size: 1}, 0},
		{[]byte{1}, archivePart{size: 1}, 1},
	}
	for _, tt := range tests {
		if _, _, err := readPart(bytes.NewReader(tt.wire), uint64(len(tt.wire)), tt.part, tt.max); !errors.Is(err, ErrCorruptArchive) {
			t.Errorf("readPart(%x) error = %v", tt.wire, err)
		}
	}
}

func TestReadAtFull_GivenShortReaderWithoutError_WhenRead_ThenReturnsUnexpectedEOF(t *testing.T) {
	err := readAtFull(shortReaderAt{}, make([]byte, 2), 0)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("readAtFull() error = %v, want unexpected EOF", err)
	}
	if err := readAtFull(shortReaderAt{}, nil, 0); err != nil {
		t.Fatalf("empty readAtFull() error = %v", err)
	}
}

func TestZigzagDecodeRelative_GivenRelativeAndAbsoluteValues_ThenDecodesThem(t *testing.T) {
	tests := []struct{ value, previous, want uint64 }{
		{20, 5, 20},
		{1, 10, 9},
		{4, 10, 12},
	}
	for _, tt := range tests {
		if got := zigzagDecodeRelative(tt.value, tt.previous); got != tt.want {
			t.Errorf("zigzagDecodeRelative(%d, %d) = %d, want %d", tt.value, tt.previous, got, tt.want)
		}
	}
}

type shortReaderAt struct{}

func (shortReaderAt) ReadAt(p []byte, _ int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = byte(math.MaxUint8)
	return 1, nil
}

type errorCloser struct{ err error }

func (c errorCloser) Close() error { return c.err }
