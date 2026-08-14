package agc

import (
	"errors"
	"reflect"
	"testing"
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
