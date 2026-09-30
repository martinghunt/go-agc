package agc

import (
	"bytes"
	"reflect"
	"testing"
)

func TestMergeReadRanges_GivenNearbyDuplicateAndDistantRanges_ThenCoalescesOnlyNearbyData(t *testing.T) {
	in := []readRange{
		{offset: 1000, length: 100},
		{offset: 100, length: 50},
		{offset: 180, length: 20},
		{offset: 100, length: 50},
		{offset: 5000, length: 40},
	}
	got := mergeReadRanges(in, 32, 1024)
	want := []readRange{
		{offset: 100, length: 100},
		{offset: 1000, length: 100},
		{offset: 5000, length: 40},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeReadRanges() = %#v, want %#v", got, want)
	}
}

func TestMergeReadRanges_GivenNearbyRangesExceedingMaximum_ThenKeepsSeparateRequests(t *testing.T) {
	in := []readRange{{offset: 0, length: 80}, {offset: 90, length: 80}}
	got := mergeReadRanges(in, 32, 128)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("mergeReadRanges() = %#v, want %#v", got, in)
	}
}

func TestShouldFetchWholeObject_GivenCoverageAndCacheLimits_ThenUsesConfiguredCutoff(t *testing.T) {
	tests := []struct {
		name         string
		plannedBytes int64
		objectBytes  int64
		cacheBytes   int64
		fraction     float64
		want         bool
	}{
		{name: "at cutoff", plannedBytes: 800, objectBytes: 1000, cacheBytes: 1000, fraction: 0.8, want: true},
		{name: "below cutoff", plannedBytes: 799, objectBytes: 1000, cacheBytes: 1000, fraction: 0.8},
		{name: "object exceeds cache", plannedBytes: 900, objectBytes: 1000, cacheBytes: 999, fraction: 0.8},
		{name: "disabled", plannedBytes: 1000, objectBytes: 1000, cacheBytes: 1000, fraction: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := shouldFetchWholeObject(test.plannedBytes, test.objectBytes, test.cacheBytes, test.fraction)
			if got != test.want {
				t.Fatalf("shouldFetchWholeObject() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPrefetchDescriptors_GivenRepeatedReferencesAndPackedSequences_ThenSelectsAndDeduplicatesParts(t *testing.T) {
	reader := &recordingPrefetchReader{}
	a := &Archive{
		r:         reader,
		batchSize: 2,
		index: archiveIndex{
			dataEnd: 1000,
			streams: map[string]archiveStream{
				"xGr": {parts: []archivePart{{offset: 100, size: 10}}},
				"xGd": {parts: []archivePart{{offset: 200, size: 20}, {offset: 300, size: 30}}},
				"x0d": {parts: []archivePart{{offset: 400, size: 40}, {offset: 500, size: 50}}},
			},
		},
	}
	descriptors := []segmentDescriptor{
		{groupID: 16, inGroupID: 3},
		{groupID: 16, inGroupID: 3},
		{groupID: 16, inGroupID: 0},
		{groupID: 0, inGroupID: 2},
	}
	if err := a.prefetchDescriptorsLocked(descriptors); err != nil {
		t.Fatal(err)
	}
	want := map[readRange]bool{
		{offset: 100, length: 19}: true,
		{offset: 300, length: 39}: true,
		{offset: 500, length: 59}: true,
	}
	if len(reader.ranges) != len(want) {
		t.Fatalf("Prefetch() got %v, want %v", reader.ranges, want)
	}
	for _, got := range reader.ranges {
		if !want[got] {
			t.Errorf("unexpected prefetched range %#v", got)
		}
	}
}

type recordingPrefetchReader struct {
	bytes.Reader
	ranges []readRange
}

func (r *recordingPrefetchReader) Prefetch(ranges []readRange) error {
	r.ranges = append(r.ranges, ranges...)
	return nil
}
