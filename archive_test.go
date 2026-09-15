package agc_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/martinghunt/go-agc"
)

func TestArchive_GivenUpstreamToyArchive_WhenOpened_ThenIdentifiesV3SamplesAndReference(t *testing.T) {
	data := toyArchive(t)
	path := filepath.Join(t.TempDir(), "toy_ex.agc")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := agc.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if got, want := a.Version(), (agc.Version{Major: 3, Minor: 0}); got != want {
		t.Errorf("Version() = %+v, want %+v", got, want)
	}

	samples, err := a.Samples()
	if err != nil {
		t.Fatalf("Samples() error = %v", err)
	}
	wantSamples := []agc.Sample{{Name: "ref"}, {Name: "a"}, {Name: "b"}, {Name: "c"}}
	if !reflect.DeepEqual(samples, wantSamples) {
		t.Errorf("Samples() = %#v, want %#v", samples, wantSamples)
	}

	reference, err := a.ReferenceSample()
	if err != nil {
		t.Fatalf("ReferenceSample() error = %v", err)
	}
	if reference.Name != "ref" {
		t.Errorf("ReferenceSample() = %q, want ref", reference.Name)
	}
}

func TestArchive_GivenGeneratedV3Fixture_WhenSampleCountCrossesOneByteBoundary_ThenListsEveryIdentity(t *testing.T) {
	want := make([]string, 130)
	for i := range want {
		want[i] = fmt.Sprintf("sample-%03d", i)
	}
	data := generatedV3Archive(t, 7, want)

	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("OpenReaderAt() error = %v", err)
	}
	samples, err := a.Samples()
	if err != nil {
		t.Fatalf("Samples() error = %v", err)
	}
	got := make([]string, len(samples))
	for i := range samples {
		got[i] = samples[i].Name
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sample names do not survive generated fixture: got %d names", len(got))
	}
	if gotVersion := a.Version(); gotVersion != (agc.Version{Major: 3, Minor: 7}) {
		t.Errorf("Version() = %+v, want 3.7", gotVersion)
	}
}

func TestArchive_GivenReaderAt_WhenOpeningAndListing_ThenReadsLazily(t *testing.T) {
	data := toyArchive(t)
	r := &countingReaderAt{r: bytes.NewReader(data)}

	a, err := agc.OpenReaderAt(r, int64(len(data)))
	if err != nil {
		t.Fatalf("OpenReaderAt() error = %v", err)
	}
	openedBytes := r.bytesRead
	if openedBytes >= int64(len(data)) {
		t.Fatalf("opening read %d bytes from a %d-byte archive; want less than whole archive", openedBytes, len(data))
	}

	if _, err := a.Samples(); err != nil {
		t.Fatalf("Samples() error = %v", err)
	}
	if r.bytesRead <= openedBytes {
		t.Errorf("Samples() read no additional data; collection-samples should be lazy")
	}
}

func TestArchive_GivenClosedArchive_WhenReading_ThenReturnsErrClosed(t *testing.T) {
	a, err := agc.OpenReaderAt(bytes.NewReader(toyArchive(t)), int64(len(toyArchive(t))))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Samples(); !errors.Is(err, agc.ErrClosed) {
		t.Errorf("Samples() error = %v, want ErrClosed", err)
	}
}

func TestArchive_GivenClosedArchive_WhenUsingEveryReadOperation_ThenReturnsErrClosed(t *testing.T) {
	data := toyArchive(t)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		call func() error
	}{
		{"Contigs", func() error { _, err := a.Contigs(agc.Sample{Name: "ref"}); return err }},
		{"Contig", func() error { _, err := a.Contig(agc.Sample{Name: "ref"}, "chr1"); return err }},
		{"IterateSample", func() error { return a.IterateSample(agc.Sample{Name: "ref"}, func(agc.Contig) error { return nil }) }},
		{"IterateAll", func() error { return a.IterateAll(func(agc.Contig) error { return nil }) }},
	}
	for _, check := range checks {
		if err := check.call(); !errors.Is(err, agc.ErrClosed) {
			t.Errorf("%s error = %v, want ErrClosed", check.name, err)
		}
	}
}

func TestArchive_GivenInvalidCallbacks_WhenIterating_ThenRejectsThem(t *testing.T) {
	data := toyArchive(t)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.IterateSample(agc.Sample{Name: "ref"}, nil); err == nil {
		t.Error("IterateSample(nil) returned nil error")
	}
	if err := a.IterateAll(nil); err == nil {
		t.Error("IterateAll(nil) returned nil error")
	}
}

func TestArchive_GivenNoSamples_WhenIdentifyingReference_ThenRejectsArchive(t *testing.T) {
	data := generatedV3Archive(t, 0, nil)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ReferenceSample(); !errors.Is(err, agc.ErrCorruptArchive) {
		t.Fatalf("ReferenceSample() error = %v, want ErrCorruptArchive", err)
	}
}

func TestArchive_GivenDuplicateOrEmptySampleIdentity_WhenListing_ThenRejectsIt(t *testing.T) {
	for _, names := range [][]string{{"same", "same"}, {""}} {
		data := generatedV3Archive(t, 0, names)
		a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Samples(); !errors.Is(err, agc.ErrCorruptArchive) {
			t.Errorf("Samples(%q) error = %v, want ErrCorruptArchive", names, err)
		}
	}
}

func TestOpen_GivenMissingPath_ThenReturnsFilesystemError(t *testing.T) {
	if _, err := agc.Open(filepath.Join(t.TempDir(), "missing.agc")); err == nil {
		t.Fatal("Open() returned nil error")
	}
}

func TestOpenReaderAt_GivenUnsupportedMajorVersion_ThenRejectsIt(t *testing.T) {
	data := toyArchive(t)
	old := []byte("file_version_major\x003\x00")
	newValue := []byte("file_version_major\x002\x00")
	data = bytes.Replace(data, old, newValue, 1)

	_, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if !errors.Is(err, agc.ErrUnsupportedVersion) {
		t.Fatalf("OpenReaderAt() error = %v, want ErrUnsupportedVersion", err)
	}
}

func TestOpenReaderAt_GivenMalformedVersionFields_ThenRejectsThem(t *testing.T) {
	tests := []struct {
		name, old, replacement string
	}{
		{"invalid major", "file_version_major\x003\x00", "file_version_major\x00x\x00"},
		{"invalid minor", "file_version_minor\x000\x00", "file_version_minor\x00x\x00"},
		{"missing minor", "file_version_minor\x00", "file_version_minoX\x00"},
		{"duplicate key", "file_version_minor\x00", "file_version_major\x00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := toyArchive(t)
			data = bytes.Replace(data, []byte(tt.old), []byte(tt.replacement), 1)
			_, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
			if !errors.Is(err, agc.ErrCorruptArchive) {
				t.Fatalf("OpenReaderAt() error = %v, want ErrCorruptArchive", err)
			}
		})
	}
}

func TestArchive_GivenInvalidOrMissingParams_WhenListingContigs_ThenRejectsIt(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"missing params", func(data []byte) []byte {
			return bytes.Replace(data, []byte("params\x00"), []byte("paramX\x00"), 1)
		}},
		{"zero batch size", func(data []byte) []byte {
			params := []byte{31, 0, 0, 0, 20, 0, 0, 0, 2, 0, 0, 0}
			replacement := append([]byte(nil), params...)
			for i := 8; i < 12; i++ {
				replacement[i] = 0
			}
			return bytes.Replace(data, params, replacement, 1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.mutate(generatedV3ContigArchive(t))
			a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Contigs(agc.Sample{Name: "ref"}); !errors.Is(err, agc.ErrCorruptArchive) {
				t.Fatalf("Contigs() error = %v, want ErrCorruptArchive", err)
			}
		})
	}
}

func TestOpenReaderAt_GivenCorruptArchive_ThenRejectsIt(t *testing.T) {
	tests := []struct {
		name string
		data func([]byte) []byte
	}{
		{"truncated footer", func(data []byte) []byte { return data[:7] }},
		{"footer outside file", func(data []byte) []byte {
			copy(data[len(data)-8:], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f})
			return data
		}},
		{"missing version stream", func(data []byte) []byte {
			return bytes.Replace(data, []byte("file_type_info\x00"), []byte("file_type_infX\x00"), 1)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.data(toyArchive(t))
			_, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
			if !errors.Is(err, agc.ErrCorruptArchive) {
				t.Fatalf("OpenReaderAt() error = %v, want ErrCorruptArchive", err)
			}
		})
	}
}

func TestArchive_GivenCorruptSampleBlockMetadata_WhenListing_ThenRejectsIt(t *testing.T) {
	data := generatedV3Archive(t, 0, []string{"reference", "sample"})
	magic := []byte{0x28, 0xb5, 0x2f, 0xfd}
	zstdOffset := bytes.Index(data, magic)
	if zstdOffset < 2 {
		t.Fatal("generated fixture has no collection zstd frame")
	}
	data[zstdOffset-1]++ // declared decompressed size no longer matches the frame

	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("OpenReaderAt() error = %v", err)
	}
	if _, err := a.Samples(); !errors.Is(err, agc.ErrCorruptArchive) {
		t.Fatalf("Samples() error = %v, want ErrCorruptArchive", err)
	}
}

func TestArchive_GivenSampleCountAboveCap_WhenListing_ThenRejectsWithoutOversizedAllocation(t *testing.T) {
	const overCap = 1<<20 + 1 // one over collection.go's maxSamples
	raw := appendCollectionUint(nil, uint32(overCap))
	raw = append(raw, make([]byte, overCap)...) // satisfies the size-only bound so only the count cap can reject this

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	packed := encoder.EncodeAll(raw, nil)
	encoder.Close()

	data := archiveWithSamplesStream(t, packed, uint64(len(raw)))
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("OpenReaderAt() error = %v", err)
	}
	if _, err := a.Samples(); !errors.Is(err, agc.ErrCorruptArchive) {
		t.Fatalf("Samples() error = %v, want ErrCorruptArchive", err)
	}
}

func TestArchive_GivenContigCountAboveCap_WhenListingContigs_ThenRejectsWithoutOversizedAllocation(t *testing.T) {
	const overCap = 1<<24 + 1 // one over collection.go's maxContigsPerSample
	data := generatedV3ContigArchiveWithOversizedContigCount(t, overCap)

	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("OpenReaderAt() error = %v", err)
	}
	if _, err := a.Contigs(agc.Sample{Name: "only"}); !errors.Is(err, agc.ErrCorruptArchive) {
		t.Fatalf("Contigs() error = %v, want ErrCorruptArchive", err)
	}
}

func TestArchive_GivenUpstreamToyArchive_WhenListingContigs_ThenPreservesSampleAndContigIdentity(t *testing.T) {
	a, err := agc.OpenReaderAt(bytes.NewReader(toyArchive(t)), int64(len(toyArchive(t))))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		sample string
		want   []string
	}{
		{"ref", []string{"chr1", "chr2", "chr3", "seq"}},
		{"a", []string{"chr1a", "chr3a"}},
		{"b", []string{"chr1", "g h i 21", "c", "t"}},
		{"c", []string{"1", "2", "3"}},
	}
	for _, tt := range tests {
		t.Run(tt.sample, func(t *testing.T) {
			contigs, err := a.Contigs(agc.Sample{Name: tt.sample})
			if err != nil {
				t.Fatalf("Contigs() error = %v", err)
			}
			got := make([]string, len(contigs))
			for i, contig := range contigs {
				if contig.Sample.Name != tt.sample {
					t.Errorf("contig %d sample = %q, want %q", i, contig.Sample.Name, tt.sample)
				}
				got[i] = contig.Name
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("contig names = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestArchive_GivenGeneratedNameDeltaAndMultipleBatches_WhenListingContigs_ThenDecodesLazily(t *testing.T) {
	data := generatedV3ContigArchive(t)
	r := &countingReaderAt{r: bytes.NewReader(data)}
	a, err := agc.OpenReaderAt(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	first, err := a.Contigs(agc.Sample{Name: "ref"})
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := []agc.ContigInfo{
		{Sample: agc.Sample{Name: "ref"}, Name: "alpha 001 x"},
		{Sample: agc.Sample{Name: "ref"}, Name: "alpha 002 x"},
		{Sample: agc.Sample{Name: "ref"}, Name: "literal"},
	}
	if !reflect.DeepEqual(first, wantFirst) {
		t.Errorf("first batch contigs = %#v, want %#v", first, wantFirst)
	}
	afterFirst := r.bytesRead

	// "same-batch" shares the already decoded part; no archive read is needed.
	if _, err := a.Contigs(agc.Sample{Name: "same-batch"}); err != nil {
		t.Fatal(err)
	}
	if r.bytesRead != afterFirst {
		t.Errorf("same batch caused %d additional bytes to be read", r.bytesRead-afterFirst)
	}

	second, err := a.Contigs(agc.Sample{Name: "next-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []agc.ContigInfo{{Sample: agc.Sample{Name: "next-batch"}, Name: "alpha 001 x"}}; !reflect.DeepEqual(second, want) {
		t.Errorf("second batch contigs = %#v, want %#v", second, want)
	}
	if r.bytesRead <= afterFirst {
		t.Error("next batch did not lazily read another archive part")
	}
}

func TestArchive_GivenUnknownSample_WhenListingContigs_ThenReturnsTypedError(t *testing.T) {
	data := generatedV3ContigArchive(t)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Contigs(agc.Sample{Name: "missing"}); !errors.Is(err, agc.ErrSampleNotFound) {
		t.Fatalf("Contigs() error = %v, want ErrSampleNotFound", err)
	}
}

func TestArchive_GivenCorruptContigNameDelta_WhenListingContigs_ThenRejectsIt(t *testing.T) {
	data := generatedV3ContigArchiveWithDelta(t, []byte{0x80, ' ', 0x81, ' ', 0x81}) // repeat 128 bytes from a shorter prior component
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Contigs(agc.Sample{Name: "ref"}); !errors.Is(err, agc.ErrCorruptArchive) {
		t.Fatalf("Contigs() error = %v, want ErrCorruptArchive", err)
	}
}

func TestArchive_GivenUpstreamToyArchive_WhenRetrievingNamedContigs_ThenDecodesSequences(t *testing.T) {
	data := toyArchive(t)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		sample   string
		name     string
		fullName string
		sequence string
	}{
		{"ref", "chr1", "chr1", "AGCTAGCTAGCTAGCT"},
		{"ref", "chr2", "chr2", "TAAAAAAAAAAATTT"},
		{"ref", "chr3", "chr3", "TGGGGGGGGGGTTT"},
		{"ref", "seq", "seq", "TGTGTGTGTG"},
		{"a", "chr1a", "chr1a", "CTGAGCTGACTGA"},
		{"a", "chr3a", "chr3a", "AGTTTAGCT"},
		{"b", "chr1", "chr1", "AAAAAAAAA"},
		{"b", "g", "g h i 21", "GGGAGGG"},
		{"b", "c", "c", "CCCCCCCCC"},
		{"b", "t", "t", "TTTTTTT"},
		{"c", "1", "1", "TGTGTGTGTGTG"},
		{"c", "2", "2", "ACACACACA"},
		{"c", "3", "3", "TTTTCCCGGGAAAAAA"},
	}
	for _, tt := range tests {
		t.Run(tt.sample+"/"+tt.name, func(t *testing.T) {
			contig, err := a.Contig(agc.Sample{Name: tt.sample}, tt.name)
			if err != nil {
				t.Fatalf("Contig() error = %v", err)
			}
			if contig.Sample.Name != tt.sample || contig.Name != tt.fullName || string(contig.Sequence) != tt.sequence {
				t.Errorf("Contig() = sample %q, name %q, sequence %q; want %q, %q, %q",
					contig.Sample.Name, contig.Name, contig.Sequence, tt.sample, tt.fullName, tt.sequence)
			}
		})
	}
}

func TestArchive_GivenRepeatedContigRetrieval_WhenDecoding_ThenReusesCachedSegmentData(t *testing.T) {
	data := toyArchive(t)
	r := &countingReaderAt{r: bytes.NewReader(data)}
	a, err := agc.OpenReaderAt(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Contig(agc.Sample{Name: "ref"}, "chr1"); err != nil {
		t.Fatalf("Contig() error = %v", err)
	}
	afterFirst := r.bytesRead

	if _, err := a.Contig(agc.Sample{Name: "ref"}, "chr1"); err != nil {
		t.Fatalf("Contig() error = %v", err)
	}
	if r.bytesRead != afterFirst {
		t.Errorf("decoding an already-decoded contig again read %d additional bytes, want 0: segment packs and references should be cached across Contig calls", r.bytesRead-afterFirst)
	}
}

func TestArchive_GivenMissingContig_WhenRetrieving_ThenReturnsTypedError(t *testing.T) {
	data := toyArchive(t)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Contig(agc.Sample{Name: "ref"}, "missing"); !errors.Is(err, agc.ErrContigNotFound) {
		t.Fatalf("Contig() error = %v, want ErrContigNotFound", err)
	}
}

func TestArchive_GivenSample_WhenIterating_ThenYieldsContigsInArchiveOrder(t *testing.T) {
	data := toyArchive(t)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	err = a.IterateSample(agc.Sample{Name: "b"}, func(contig agc.Contig) error {
		got = append(got, contig.Sample.Name+"/"+contig.Name+"="+string(contig.Sequence))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"b/chr1=AAAAAAAAA", "b/g h i 21=GGGAGGG", "b/c=CCCCCCCCC", "b/t=TTTTTTT"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("IterateSample() = %#v, want %#v", got, want)
	}
}

func TestContigReader_GivenSample_WhenReadToEnd_ThenYieldsContigsInArchiveOrder(t *testing.T) {
	data := toyArchive(t)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.NewContigReader(agc.Sample{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for {
		contig, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, contig.Sample.Name+"/"+contig.Name+"="+string(contig.Sequence))
	}
	want := []string{"b/chr1=AAAAAAAAA", "b/g h i 21=GGGAGGG", "b/c=CCCCCCCCC", "b/t=TTTTTTT"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ContigReader = %#v, want %#v", got, want)
	}
	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Errorf("Read() after EOF error = %v, want io.EOF", err)
	}
}

func TestNewContigReader_GivenUnavailableSampleOrClosedArchive_WhenOpened_ThenReturnsTypedError(t *testing.T) {
	data := toyArchive(t)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.NewContigReader(agc.Sample{Name: "missing"}); !errors.Is(err, agc.ErrSampleNotFound) {
		t.Errorf("NewContigReader(missing) error = %v, want ErrSampleNotFound", err)
	}
	r, err := a.NewContigReader(agc.Sample{Name: "ref"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(); !errors.Is(err, agc.ErrClosed) {
		t.Errorf("Read() after archive close error = %v, want ErrClosed", err)
	}
	if _, err := a.NewContigReader(agc.Sample{Name: "ref"}); !errors.Is(err, agc.ErrClosed) {
		t.Errorf("NewContigReader(closed) error = %v, want ErrClosed", err)
	}
}

func TestArchive_GivenAllSamples_WhenIterating_ThenYieldsArchiveOrderAndCanStop(t *testing.T) {
	data := toyArchive(t)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	err = a.IterateAll(func(contig agc.Contig) error {
		got = append(got, contig.Sample.Name+"/"+contig.Name)
		return nil
	})
	if err != nil {
		t.Fatalf("IterateAll() error = %v", err)
	}
	want := []string{
		"ref/chr1", "ref/chr2", "ref/chr3", "ref/seq",
		"a/chr1a", "a/chr3a",
		"b/chr1", "b/g h i 21", "b/c", "b/t",
		"c/1", "c/2", "c/3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("IterateAll() = %#v, want %#v", got, want)
	}

	stop := errors.New("stop iteration")
	count := 0
	err = a.IterateAll(func(agc.Contig) error {
		count++
		if count == 5 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || count != 5 {
		t.Fatalf("early IterateAll() = count %d, error %v; want 5 and stop error", count, err)
	}
}

func TestArchive_GivenConcurrentReaders_WhenRetrievingContigs_ThenResultsRemainIndependent(t *testing.T) {
	data := toyArchive(t)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{"chr1": "AGCTAGCTAGCTAGCT", "chr2": "TAAAAAAAAAAATTT", "chr3": "TGGGGGGGGGGTTT", "seq": "TGTGTGTGTG"}
	for name, want := range tests {
		name, want := name, want
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			contig, err := a.Contig(agc.Sample{Name: "ref"}, name)
			if err != nil {
				t.Fatal(err)
			}
			if string(contig.Sequence) != want {
				t.Errorf("sequence = %q, want %q", contig.Sequence, want)
			}
		})
	}
}

func TestArchive_GivenReferenceGeneratedV3Fixture_WhenRetrieving_ThenMatchesInput(t *testing.T) {
	agcExe := referenceAGC(t)
	tmp := t.TempDir()
	refSequence := strings.Repeat("ACGT", 70) + "NNNN" + strings.Repeat("TGCA", 30)
	sampleSequence := refSequence[:145] + "TTTT" + refSequence[149:]
	refPath := filepath.Join(tmp, "generated-ref.fa")
	samplePath := filepath.Join(tmp, "generated-sample.fa")
	if err := os.WriteFile(refPath, []byte(">ref-contig description\n"+refSequence+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(samplePath, []byte(">sample-contig description\n"+sampleSequence+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(tmp, "generated.agc")
	if out, err := exec.Command(agcExe, "create", "-b", "1", "-k", "17", "-l", "15", "-s", "100", "-t", "1", "-o", archivePath, refPath, samplePath).CombinedOutput(); err != nil {
		t.Fatalf("reference agc create: %v\n%s", err, out)
	}

	a, err := agc.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	tests := []struct {
		sample, query, fullName, sequence string
	}{
		{"generated-ref", "ref-contig", "ref-contig description", refSequence},
		{"generated-sample", "sample-contig", "sample-contig description", sampleSequence},
	}
	for _, tt := range tests {
		contig, err := a.Contig(agc.Sample{Name: tt.sample}, tt.query)
		if err != nil {
			t.Fatalf("Contig(%q, %q): %v", tt.sample, tt.query, err)
		}
		if contig.Name != tt.fullName || string(contig.Sequence) != tt.sequence {
			t.Errorf("generated contig %s differs from input", tt.query)
		}
	}
}

func TestArchive_GivenReferenceAGCExecutable_WhenListingToyArchive_ThenOutputsAgree(t *testing.T) {
	agcExe := os.Getenv("AGC_REFERENCE")
	if agcExe == "" {
		var err error
		agcExe, err = exec.LookPath("agc")
		if err != nil {
			t.Skip("set AGC_REFERENCE to cross-check against the reference executable")
		}
	}
	data := toyArchive(t)
	path := filepath.Join(t.TempDir(), "toy_ex.agc")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := agc.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	samples, err := a.Samples()
	if err != nil {
		t.Fatal(err)
	}
	gotNames := make(map[string]bool, len(samples))
	for _, sample := range samples {
		gotNames[sample.Name] = true
	}

	out, err := exec.Command(agcExe, "listset", path).Output()
	if err != nil {
		t.Fatalf("reference agc listset: %v", err)
	}
	wantNames := make(map[string]bool)
	for _, name := range strings.Fields(string(out)) {
		wantNames[name] = true
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("sample identities = %#v, reference CLI = %#v", gotNames, wantNames)
	}

	out, err = exec.Command(agcExe, "listref", path).Output()
	if err != nil {
		t.Fatalf("reference agc listref: %v", err)
	}
	ref, err := a.ReferenceSample()
	if err != nil {
		t.Fatal(err)
	}
	if ref.Name != strings.TrimSpace(string(out)) {
		t.Errorf("reference = %q, reference CLI = %q", ref.Name, strings.TrimSpace(string(out)))
	}

	args := []string{"listctg", path}
	var wantCatalogue strings.Builder
	for _, sample := range samples {
		args = append(args, sample.Name)
		fmt.Fprintln(&wantCatalogue, sample.Name)
		contigs, err := a.Contigs(sample)
		if err != nil {
			t.Fatal(err)
		}
		for _, contig := range contigs {
			fmt.Fprintf(&wantCatalogue, "   %s\n", contig.Name)
		}
	}
	out, err = exec.Command(agcExe, args...).Output()
	if err != nil {
		t.Fatalf("reference agc listctg: %v", err)
	}
	if string(out) != wantCatalogue.String() {
		t.Errorf("contig catalogue differs from reference CLI\ngot:\n%s\nwant:\n%s", wantCatalogue.String(), out)
	}

	for _, sample := range samples {
		contigs, err := a.Contigs(sample)
		if err != nil {
			t.Fatal(err)
		}
		for _, info := range contigs {
			query := shortFixtureContigName(info.Name) + "@" + sample.Name
			out, err := exec.Command(agcExe, "getctg", "-l", "0", path, query).Output()
			if err != nil {
				t.Fatalf("reference agc getctg %q: %v", query, err)
			}
			contig, err := a.Contig(sample, info.Name)
			if err != nil {
				t.Fatal(err)
			}
			want := ">" + contig.Name + "\n" + string(contig.Sequence) + "\n"
			if string(out) != want {
				t.Errorf("decoded %s differs from reference CLI\ngot: %q\nwant: %q", query, want, out)
			}
		}
	}
}

func BenchmarkOpenToyArchive(b *testing.B) {
	data := toyArchive(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			b.Fatal(err)
		}
		_ = a.Close()
	}
}

func BenchmarkListSamplesToyArchive(b *testing.B) {
	data := toyArchive(b)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.Samples(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListContigsToyArchive(b *testing.B) {
	data := toyArchive(b)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		b.Fatal(err)
	}
	if _, err := a.Contigs(agc.Sample{Name: "ref"}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.Contigs(agc.Sample{Name: "ref"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRetrieveContigToyArchive(b *testing.B) {
	data := toyArchive(b)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.Contig(agc.Sample{Name: "ref"}, "chr1"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadSampleToyArchive(b *testing.B) {
	data := toyArchive(b)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		b.Fatal(err)
	}
	sample := agc.Sample{Name: "b"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := a.NewContigReader(sample)
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := r.Read(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkIterateAllToyArchive(b *testing.B) {
	data := toyArchive(b)
	a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := a.IterateAll(func(agc.Contig) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzArchiveReader(f *testing.F) {
	f.Add(toyArchive(f))
	f.Add([]byte("not an AGC archive"))
	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := agc.OpenReaderAt(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		defer a.Close()
		samples, err := a.Samples()
		if err != nil {
			return
		}
		for _, sample := range samples {
			_, _ = a.Contigs(sample)
		}
	})
}

func toyArchive(tb testing.TB) []byte {
	tb.Helper()
	encoded, err := os.ReadFile(filepath.Join("testdata", "toy_ex.agc.b64"))
	if err != nil {
		tb.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		tb.Fatal(err)
	}
	return data
}

func referenceAGC(tb testing.TB) string {
	tb.Helper()
	agcExe := os.Getenv("AGC_REFERENCE")
	if agcExe != "" {
		return agcExe
	}
	var err error
	agcExe, err = exec.LookPath("agc")
	if err != nil {
		tb.Skip("set AGC_REFERENCE to cross-check against the reference executable")
	}
	return agcExe
}

func shortFixtureContigName(name string) string {
	if i := strings.IndexAny(name, " \n\r\t"); i >= 0 {
		return name[:i]
	}
	return name
}

type countingReaderAt struct {
	r         *bytes.Reader
	bytesRead int64
}

type fixturePart struct {
	offset uint64
	size   uint64
}

type fixtureStream struct {
	name string
	part fixturePart
}

type fixtureIndexedStream struct {
	name  string
	parts []fixturePart
}

func generatedV3Archive(tb testing.TB, minor uint32, sampleNames []string) []byte {
	tb.Helper()
	fileInfo := appendCString(nil, "file_version_major")
	fileInfo = appendCString(fileInfo, "3")
	fileInfo = appendCString(fileInfo, "file_version_minor")
	fileInfo = appendCString(fileInfo, fmt.Sprint(minor))

	collection := appendCollectionUint(nil, uint32(len(sampleNames)))
	for _, name := range sampleNames {
		collection = appendCString(collection, name)
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		tb.Fatal(err)
	}
	packedSamples := encoder.EncodeAll(collection, nil)
	encoder.Close()

	var archive []byte
	streams := make([]fixtureStream, 0, 2)
	addPart := func(name string, data []byte, metadata uint64) {
		offset := uint64(len(archive))
		archive = appendArchiveUint(archive, metadata)
		archive = append(archive, data...)
		streams = append(streams, fixtureStream{name: name, part: fixturePart{offset: offset, size: uint64(len(data))}})
	}
	addPart("file_type_info", fileInfo, 2)
	addPart("collection-samples", packedSamples, uint64(len(collection)))

	var footer []byte
	footer = appendArchiveUint(footer, uint64(len(streams)))
	for _, stream := range streams {
		footer = appendCString(footer, stream.name)
		footer = appendArchiveUint(footer, 1) // parts
		footer = appendArchiveUint(footer, 0) // aggregate raw size is informational
		footer = appendArchiveUint(footer, stream.part.offset)
		footer = appendArchiveUint(footer, stream.part.size)
	}
	archive = append(archive, footer...)
	archive = binary.LittleEndian.AppendUint64(archive, uint64(len(footer)))
	return archive
}

func generatedV3ContigArchive(tb testing.TB) []byte {
	return generatedV3ContigArchiveWithDelta(tb, []byte{0x81, ' ', 0xfe, '2', ' ', 0x81})
}

func generatedV3ContigArchiveWithDelta(tb testing.TB, secondContigDelta []byte) []byte {
	tb.Helper()
	sampleNames := []string{"ref", "same-batch", "next-batch"}
	fileInfo := appendCString(nil, "file_version_major")
	fileInfo = appendCString(fileInfo, "3")
	fileInfo = appendCString(fileInfo, "file_version_minor")
	fileInfo = appendCString(fileInfo, "0")

	collectionSamples := appendCollectionUint(nil, uint32(len(sampleNames)))
	for _, name := range sampleNames {
		collectionSamples = appendCString(collectionSamples, name)
	}

	// Batch zero has two samples. The second ref contig is the upstream v3
	// delta representation of "alpha 002 x" relative to "alpha 001 x".
	batch0 := appendCollectionUint(nil, 2)
	batch0 = appendCollectionUint(batch0, 3)
	batch0 = appendCString(batch0, "alpha 001 x")
	batch0 = appendCStringBytes(batch0, secondContigDelta)
	batch0 = appendCString(batch0, "literal")
	batch0 = appendCollectionUint(batch0, 1)
	batch0 = appendCString(batch0, "alpha 001 x")
	batch1 := appendCollectionUint(nil, 1)
	batch1 = appendCollectionUint(batch1, 1)
	batch1 = appendCString(batch1, "alpha 001 x")

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		tb.Fatal(err)
	}
	packedSamples := encoder.EncodeAll(collectionSamples, nil)
	packedBatch0 := encoder.EncodeAll(batch0, nil)
	packedBatch1 := encoder.EncodeAll(batch1, nil)
	encoder.Close()

	params := make([]byte, 16)
	binary.LittleEndian.PutUint32(params[0:4], 31)
	binary.LittleEndian.PutUint32(params[4:8], 20)
	binary.LittleEndian.PutUint32(params[8:12], 2) // batch cardinality
	binary.LittleEndian.PutUint32(params[12:16], 60_000)

	var archive []byte
	streams := make([]fixtureIndexedStream, 0, 4)
	addStream := func(name string, dataParts [][]byte, metadata []uint64) {
		stream := fixtureIndexedStream{name: name, parts: make([]fixturePart, 0, len(dataParts))}
		for i, data := range dataParts {
			offset := uint64(len(archive))
			archive = appendArchiveUint(archive, metadata[i])
			archive = append(archive, data...)
			stream.parts = append(stream.parts, fixturePart{offset: offset, size: uint64(len(data))})
		}
		streams = append(streams, stream)
	}
	addStream("file_type_info", [][]byte{fileInfo}, []uint64{2})
	addStream("collection-samples", [][]byte{packedSamples}, []uint64{uint64(len(collectionSamples))})
	addStream("params", [][]byte{params}, []uint64{0})
	addStream("collection-contigs", [][]byte{packedBatch0, packedBatch1}, []uint64{uint64(len(batch0)), uint64(len(batch1))})

	var footer []byte
	footer = appendArchiveUint(footer, uint64(len(streams)))
	for _, stream := range streams {
		footer = appendCString(footer, stream.name)
		footer = appendArchiveUint(footer, uint64(len(stream.parts)))
		footer = appendArchiveUint(footer, 0)
		for _, part := range stream.parts {
			footer = appendArchiveUint(footer, part.offset)
			footer = appendArchiveUint(footer, part.size)
		}
	}
	archive = append(archive, footer...)
	return binary.LittleEndian.AppendUint64(archive, uint64(len(footer)))
}

// archiveWithSamplesStream builds a minimal v3 archive whose collection-samples
// stream is exactly the caller-supplied packed bytes, so tests can assert on
// declared counts that disagree with the actual sample list.
func archiveWithSamplesStream(tb testing.TB, packedSamples []byte, rawSize uint64) []byte {
	tb.Helper()
	fileInfo := appendCString(nil, "file_version_major")
	fileInfo = appendCString(fileInfo, "3")
	fileInfo = appendCString(fileInfo, "file_version_minor")
	fileInfo = appendCString(fileInfo, "0")

	var archive []byte
	streams := make([]fixtureStream, 0, 2)
	addPart := func(name string, data []byte, metadata uint64) {
		offset := uint64(len(archive))
		archive = appendArchiveUint(archive, metadata)
		archive = append(archive, data...)
		streams = append(streams, fixtureStream{name: name, part: fixturePart{offset: offset, size: uint64(len(data))}})
	}
	addPart("file_type_info", fileInfo, 2)
	addPart("collection-samples", packedSamples, rawSize)

	var footer []byte
	footer = appendArchiveUint(footer, uint64(len(streams)))
	for _, stream := range streams {
		footer = appendCString(footer, stream.name)
		footer = appendArchiveUint(footer, 1) // parts
		footer = appendArchiveUint(footer, 0) // aggregate raw size is informational
		footer = appendArchiveUint(footer, stream.part.offset)
		footer = appendArchiveUint(footer, stream.part.size)
	}
	archive = append(archive, footer...)
	return binary.LittleEndian.AppendUint64(archive, uint64(len(footer)))
}

// generatedV3ContigArchiveWithOversizedContigCount builds a one-sample, one-batch
// v3 archive whose collection-contigs batch declares contigCount for its only
// sample, followed by enough padding that the size-only bound alone would
// accept it.
func generatedV3ContigArchiveWithOversizedContigCount(tb testing.TB, contigCount uint32) []byte {
	tb.Helper()
	sampleNames := []string{"only"}
	fileInfo := appendCString(nil, "file_version_major")
	fileInfo = appendCString(fileInfo, "3")
	fileInfo = appendCString(fileInfo, "file_version_minor")
	fileInfo = appendCString(fileInfo, "0")

	collectionSamples := appendCollectionUint(nil, uint32(len(sampleNames)))
	for _, name := range sampleNames {
		collectionSamples = appendCString(collectionSamples, name)
	}

	batch0 := appendCollectionUint(nil, uint32(len(sampleNames)))
	batch0 = appendCollectionUint(batch0, contigCount)
	batch0 = append(batch0, make([]byte, contigCount)...)

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		tb.Fatal(err)
	}
	packedSamples := encoder.EncodeAll(collectionSamples, nil)
	packedBatch0 := encoder.EncodeAll(batch0, nil)
	encoder.Close()

	params := make([]byte, 16)
	binary.LittleEndian.PutUint32(params[0:4], 31)
	binary.LittleEndian.PutUint32(params[4:8], 20)
	binary.LittleEndian.PutUint32(params[8:12], uint32(len(sampleNames))) // batch cardinality
	binary.LittleEndian.PutUint32(params[12:16], 60_000)

	var archive []byte
	streams := make([]fixtureIndexedStream, 0, 4)
	addStream := func(name string, dataParts [][]byte, metadata []uint64) {
		stream := fixtureIndexedStream{name: name, parts: make([]fixturePart, 0, len(dataParts))}
		for i, data := range dataParts {
			offset := uint64(len(archive))
			archive = appendArchiveUint(archive, metadata[i])
			archive = append(archive, data...)
			stream.parts = append(stream.parts, fixturePart{offset: offset, size: uint64(len(data))})
		}
		streams = append(streams, stream)
	}
	addStream("file_type_info", [][]byte{fileInfo}, []uint64{2})
	addStream("collection-samples", [][]byte{packedSamples}, []uint64{uint64(len(collectionSamples))})
	addStream("params", [][]byte{params}, []uint64{0})
	addStream("collection-contigs", [][]byte{packedBatch0}, []uint64{uint64(len(batch0))})

	var footer []byte
	footer = appendArchiveUint(footer, uint64(len(streams)))
	for _, stream := range streams {
		footer = appendCString(footer, stream.name)
		footer = appendArchiveUint(footer, uint64(len(stream.parts)))
		footer = appendArchiveUint(footer, 0)
		for _, part := range stream.parts {
			footer = appendArchiveUint(footer, part.offset)
			footer = appendArchiveUint(footer, part.size)
		}
	}
	archive = append(archive, footer...)
	return binary.LittleEndian.AppendUint64(archive, uint64(len(footer)))
}

func appendCString(dst []byte, value string) []byte {
	dst = append(dst, value...)
	return append(dst, 0)
}

func appendCStringBytes(dst, value []byte) []byte {
	dst = append(dst, value...)
	return append(dst, 0)
}

func appendArchiveUint(dst []byte, value uint64) []byte {
	n := 0
	for x := value; x != 0; x >>= 8 {
		n++
	}
	dst = append(dst, byte(n))
	for shift := (n - 1) * 8; shift >= 0 && n != 0; shift -= 8 {
		dst = append(dst, byte(value>>shift))
	}
	return dst
}

func appendCollectionUint(dst []byte, value uint32) []byte {
	const (
		threshold1 = 1 << 7
		threshold2 = threshold1 + (1 << 14)
		threshold3 = threshold2 + (1 << 21)
		threshold4 = threshold3 + (1 << 28)
	)
	switch {
	case value < threshold1:
		return append(dst, byte(value))
	case value < threshold2:
		value -= threshold1
		return append(dst, 0x80+byte(value>>8), byte(value))
	case value < threshold3:
		value -= threshold2
		return append(dst, 0xc0+byte(value>>16), byte(value>>8), byte(value))
	case value < threshold4:
		value -= threshold3
		return append(dst, 0xe0+byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
	default:
		value -= threshold4
		return append(dst, 0xf0, byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
	}
}

func (r *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.r.ReadAt(p, off)
	r.bytesRead += int64(n)
	return n, err
}
