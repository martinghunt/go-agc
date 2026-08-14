package agc_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
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

func appendCString(dst []byte, value string) []byte {
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
