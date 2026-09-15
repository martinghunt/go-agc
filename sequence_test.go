package agc

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDecodeLZ_GivenV3Tokens_WhenDecoded_ThenReconstructsReferenceLiteralsMatchesAndNRuns(t *testing.T) {
	reference := []byte{0, 1, 2, 3, 0, 1}
	tests := []struct {
		name    string
		encoded []byte
		want    []byte
	}{
		{"whole-reference match", []byte("0."), reference},
		{"literal and same-position literal", []byte{'B', '!'}, []byte{1, 1}},
		{"bounded match", []byte("1,0."), []byte{1, 2, 3}},
		{"N run", []byte{30, '2', 4}, []byte{4, 4, 4, 4, 4, 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeLZ(reference, tt.encoded, 3, uint32(len(tt.want)))
			if err != nil {
				t.Fatalf("decodeLZ() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("decodeLZ() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNamedContigID_GivenRepeatedLookups_WhenResolvingNames_ThenCachesOneSampleAndKeepsFirstShortName(t *testing.T) {
	a := &Archive{namedContigSampleID: -1}
	names := []string{"alpha first", "alpha second", "beta description"}

	if got := a.namedContigID(4, names, "alpha"); got != 0 {
		t.Fatalf("namedContigID(alpha) = %d, want first matching index 0", got)
	}
	if got := a.namedContigID(4, names, "beta"); got != 2 {
		t.Fatalf("namedContigID(beta) = %d, want 2", got)
	}
	if a.namedContigSampleID != 4 || len(a.namedContigIDs) != 2 {
		t.Fatalf("cached lookup = sample %d with %d names, want sample 4 with 2 names", a.namedContigSampleID, len(a.namedContigIDs))
	}

	otherNames := []string{"gamma description"}
	if got := a.namedContigID(9, otherNames, "gamma"); got != 0 {
		t.Fatalf("namedContigID(gamma) = %d, want 0", got)
	}
	if got := a.namedContigID(9, otherNames, "alpha"); got != -1 {
		t.Fatalf("namedContigID(alpha) after changing sample = %d, want -1", got)
	}
}

func TestDecodeLZ_GivenCorruptTokens_WhenDecoded_ThenReturnsCorruptArchive(t *testing.T) {
	tests := [][]byte{
		[]byte("99."),
		[]byte("0,999."),
		[]byte("0,1"),
		{30, '9'},
		{'!', '!', '!', '!'},
	}
	for _, encoded := range tests {
		if _, err := decodeLZ([]byte{0, 1, 2}, encoded, 3, 8); !errors.Is(err, ErrCorruptArchive) {
			t.Errorf("decodeLZ(%q) error = %v, want ErrCorruptArchive", encoded, err)
		}
	}
}

func TestTuplesToBytes_GivenPackedDNA_WhenDecoded_ThenExpandsSymbols(t *testing.T) {
	// Width 4/base 4: 0b00011011 expands to A,C,G,T. The dummy tail
	// tuple is ignored because the marker says there are no trailing symbols.
	got, err := tuplesToBytes([]byte{27, 0, 0x40}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{0, 1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("tuplesToBytes() = %v, want %v", got, want)
	}
}

func TestTuplesToBytes_GivenEveryV3PackingWidth_WhenDecoded_ThenExpandsSymbols(t *testing.T) {
	tests := []struct {
		name     string
		tuples   []byte
		expected []byte
	}{
		{"plain width", []byte{5, 4, 3, 0x10}, []byte{5, 4, 3}},
		{"base16 width two", []byte{18, 3, 0x21}, []byte{1, 2, 3}},
		{"base6 width three", []byte{51, 4, 0x31}, []byte{1, 2, 3, 4}},
		{"base4 width four", []byte{27, 0, 0x40}, []byte{0, 1, 2, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tuplesToBytes(tt.tuples, uint64(len(tt.expected)))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("tuplesToBytes() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestSegmentDecoder_GivenCompressedReferenceAndDelta_WhenDecoding_ThenUsesLZReferencePath(t *testing.T) {
	reference := []byte{0, 1, 2, 3, 0, 1, 2, 3}
	builder := newSegmentArchiveBuilder(t, 2, 3)
	builder.addCompressed("xGr", append(append([]byte(nil), reference...), 0), uint64(len(reference)))
	builder.addCompressed("xGd", append(append([]byte("0."), segmentSeparator), 0), 3)
	decoder := segmentDecoder{archive: builder.archive()}

	got, err := decoder.decodeDelta(16, 1, uint32(len(reference)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, reference) {
		t.Errorf("decodeDelta() = %v, want %v", got, reference)
	}
}

func TestSegmentDecoder_GivenTuplePackedReference_WhenLoading_ThenExpandsIt(t *testing.T) {
	builder := newSegmentArchiveBuilder(t, 2, 3)
	// Tuple 27 is A,C,G,T in base 4; the zero is the no-trailing-symbol dummy.
	builder.addCompressed("xGr", []byte{27, 0, 0x40, 1}, 4)
	decoder := segmentDecoder{archive: builder.archive()}

	got, err := decoder.reference(16)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{0, 1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("reference() = %v, want %v", got, want)
	}
}

func TestSegmentDecoder_GivenSecondRawAndDeltaPacks_WhenDecoding_ThenSelectsRequestedPart(t *testing.T) {
	t.Run("raw group", func(t *testing.T) {
		builder := newSegmentArchiveBuilder(t, 2, 3)
		builder.addPlain("x0d", []byte{0, segmentSeparator, 1, segmentSeparator})
		builder.addPlain("x0d", []byte{2, 3, segmentSeparator})
		decoder := segmentDecoder{archive: builder.archive()}
		got, err := decoder.decodeRaw(0, 2)
		if err != nil {
			t.Fatal(err)
		}
		if want := []byte{2, 3}; !reflect.DeepEqual(got, want) {
			t.Errorf("decodeRaw() = %v, want %v", got, want)
		}
	})

	t.Run("delta group", func(t *testing.T) {
		reference := []byte{0, 1, 2, 3}
		builder := newSegmentArchiveBuilder(t, 2, 3)
		builder.addPlain("xGr", reference)
		builder.addPlain("xGd", []byte{'A', segmentSeparator, 'B', segmentSeparator})
		builder.addPlain("xGd", append([]byte("0."), segmentSeparator))
		decoder := segmentDecoder{archive: builder.archive()}
		got, err := decoder.decodeDelta(16, 3, uint32(len(reference)))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, reference) {
			t.Errorf("decodeDelta() = %v, want %v", got, reference)
		}
	})
}

func TestSplitSegmentPack_GivenTerminatedAndTruncatedPacks_ThenIndexesOnlyCompletedSequences(t *testing.T) {
	tests := []struct {
		name string
		pack []byte
		want [][]byte
	}{
		{"empty pack has no sequences", nil, [][]byte{}},
		{"properly terminated", []byte{0, segmentSeparator, 1, segmentSeparator}, [][]byte{{0}, {1}}},
		{"empty sequence", []byte{segmentSeparator}, [][]byte{{}}},
		{"trailing data without a terminator is not a sequence", []byte{0, segmentSeparator, 1}, [][]byte{{0}}},
		{"no terminator at all", []byte{1, 2, 3}, [][]byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitSegmentPack(tt.pack)
			if len(got) != len(tt.want) {
				t.Fatalf("splitSegmentPack(%v) = %v, want %v", tt.pack, got, tt.want)
			}
			for i := range got {
				if !reflect.DeepEqual(got[i], tt.want[i]) {
					t.Errorf("splitSegmentPack(%v)[%d] = %v, want %v", tt.pack, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestReverseComplement_GivenCanonicalAndAmbiguousCodes_ThenMatchesAGCRules(t *testing.T) {
	sequence := []byte{0, 1, 2, 3, 4, 15}
	reverseComplement(sequence)
	if want := []byte{15, 4, 0, 1, 2, 3}; !reflect.DeepEqual(sequence, want) {
		t.Errorf("reverseComplement() = %v, want %v", sequence, want)
	}
}

func TestSequencePrimitives_GivenMalformedData_WhenDecoded_ThenReturnCorruptionErrors(t *testing.T) {
	t.Run("tuple encodings", func(t *testing.T) {
		for _, tuples := range [][]byte{nil, {1, 0x50}, {1, 0x22}, {1, 0, 0x40}} {
			if _, err := tuplesToBytes(tuples, 9); !errors.Is(err, ErrCorruptArchive) {
				t.Errorf("tuplesToBytes(%v) error = %v", tuples, err)
			}
		}
	})
	t.Run("sequence pack indexes", func(t *testing.T) {
		parts := splitSegmentPack([]byte{1, segmentSeparator})
		for _, index := range []int{-1, 1} {
			if _, err := sequenceAt(parts, index); !errors.Is(err, ErrCorruptArchive) {
				t.Errorf("sequenceAt(index %d) error = %v", index, err)
			}
		}
	})
	t.Run("zstd bounds and frame", func(t *testing.T) {
		if _, err := decodeZstdAtMost(nil, 0, 10); !errors.Is(err, ErrCorruptArchive) {
			t.Errorf("zero-bound decode error = %v", err)
		}
		if _, err := decodeZstdAtMost([]byte("bad frame"), 10, 10); !errors.Is(err, ErrCorruptArchive) {
			t.Errorf("bad-frame decode error = %v", err)
		}
	})
	t.Run("nucleotide alphabet", func(t *testing.T) {
		if _, err := nucleotideText([]byte{16}); !errors.Is(err, ErrCorruptArchive) {
			t.Errorf("nucleotideText() error = %v", err)
		}
	})
	t.Run("decimal overflow", func(t *testing.T) {
		if _, _, err := readDecimal([]byte("999999999999999999999"), 0); err == nil {
			t.Error("readDecimal() accepted overflow")
		}
	})
}

func TestSegmentDecoder_GivenMalformedOrMismatchedParts_WhenDecoding_ThenRejectsThem(t *testing.T) {
	t.Run("wrong decoded length", func(t *testing.T) {
		builder := newSegmentArchiveBuilder(t, 2, 3)
		builder.addPlain("x0d", []byte{0, segmentSeparator})
		decoder := segmentDecoder{archive: builder.archive()}
		_, err := decoder.decode(segmentDescriptor{groupID: 0, rawLength: 2})
		if !errors.Is(err, ErrCorruptArchive) {
			t.Fatalf("decode() error = %v", err)
		}
	})
	t.Run("invalid segment compression marker", func(t *testing.T) {
		builder := newSegmentArchiveBuilder(t, 2, 3)
		builder.addPart("x0d", []byte{1, 2, 9}, 2)
		decoder := segmentDecoder{archive: builder.archive()}
		if _, err := decoder.decodeRaw(0, 0); !errors.Is(err, ErrCorruptArchive) {
			t.Fatalf("decodeRaw() error = %v", err)
		}
	})
	t.Run("invalid reference marker", func(t *testing.T) {
		builder := newSegmentArchiveBuilder(t, 2, 3)
		builder.addCompressed("xGr", []byte{0, 1, 2, 9}, 3)
		decoder := segmentDecoder{archive: builder.archive()}
		if _, err := decoder.reference(16); !errors.Is(err, ErrCorruptArchive) {
			t.Fatalf("reference() error = %v", err)
		}
	})
}

func FuzzDecodeLZ(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3}, []byte("0."), uint8(4))
	f.Add([]byte{0, 1, 2, 3}, []byte{30, '2', 4}, uint8(6))
	f.Add([]byte{0, 1, 2, 3}, []byte("-999,4."), uint8(8))
	f.Fuzz(func(t *testing.T, reference, encoded []byte, size uint8) {
		// Keep allocations bounded while exercising arbitrary token streams.
		_, _ = decodeLZ(reference, encoded, 3, uint32(size))
	})
}

func FuzzTuplesToBytes(f *testing.F) {
	f.Add([]byte{27, 0, 0x40}, uint16(4))
	f.Add([]byte{1, 0x50}, uint16(1))
	f.Fuzz(func(t *testing.T, tuples []byte, expected uint16) {
		_, _ = tuplesToBytes(tuples, uint64(expected))
	})
}

type segmentArchiveBuilder struct {
	t         *testing.T
	data      []byte
	streams   map[string]archiveStream
	batchSize uint32
	minMatch  uint32
}

func newSegmentArchiveBuilder(t *testing.T, batchSize, minMatch uint32) *segmentArchiveBuilder {
	t.Helper()
	return &segmentArchiveBuilder{t: t, streams: make(map[string]archiveStream), batchSize: batchSize, minMatch: minMatch}
}

func (b *segmentArchiveBuilder) addPlain(name string, data []byte) {
	b.addPart(name, data, 0)
}

func (b *segmentArchiveBuilder) addCompressed(name string, rawWithMarker []byte, metadata uint64) {
	b.t.Helper()
	if len(rawWithMarker) == 0 {
		b.t.Fatal("compressed test part needs a marker")
	}
	marker := rawWithMarker[len(rawWithMarker)-1]
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		b.t.Fatal(err)
	}
	packed := encoder.EncodeAll(rawWithMarker[:len(rawWithMarker)-1], nil)
	encoder.Close()
	packed = append(packed, marker)
	b.addPart(name, packed, metadata)
}

func (b *segmentArchiveBuilder) addPart(name string, data []byte, metadata uint64) {
	b.t.Helper()
	offset := uint64(len(b.data))
	b.data = appendTestArchiveUint(b.data, metadata)
	b.data = append(b.data, data...)
	stream := b.streams[name]
	stream.parts = append(stream.parts, archivePart{offset: offset, size: uint64(len(data))})
	b.streams[name] = stream
}

func (b *segmentArchiveBuilder) archive() *Archive {
	return &Archive{
		r: bytes.NewReader(b.data), index: archiveIndex{streams: b.streams, dataEnd: uint64(len(b.data))},
		batchSize: b.batchSize, minMatchLen: b.minMatch,
		referenceCache: newBoundedCache[uint32, []byte](maxSegmentCacheBytes),
		packCache:      newBoundedCache[segmentPackKey, [][]byte](maxSegmentCacheBytes),
	}
}

func appendTestArchiveUint(dst []byte, value uint64) []byte {
	n := 0
	for x := value; x != 0; x >>= 8 {
		n++
	}
	dst = append(dst, byte(n))
	for shift := (n - 1) * 8; n != 0 && shift >= 0; shift -= 8 {
		dst = append(dst, byte(value>>shift))
	}
	return dst
}
