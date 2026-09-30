package agc_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/martinghunt/go-agc"
)

func TestOpenURL_GivenRangeServer_WhenReadingSample_ThenDecodesWithoutFullDownload(t *testing.T) {
	data := toyArchive(t)
	transport := &rangeTransport{data: data}
	client := &http.Client{Transport: transport}

	a, err := agc.OpenURLWithOptions(context.Background(), "https://example.invalid/genomes.agc", rangedHTTPOptions(client))
	if err != nil {
		t.Fatalf("OpenURL() error = %v", err)
	}
	defer a.Close()

	var got []string
	err = a.IterateSample(agc.Sample{Name: "b"}, func(contig agc.Contig) error {
		got = append(got, contig.Name+"="+string(contig.Sequence))
		return nil
	})
	if err != nil {
		t.Fatalf("IterateSample() error = %v", err)
	}
	want := []string{"chr1=AAAAAAAAA", "g h i 21=GGGAGGG", "c=CCCCCCCCC", "t=TTTTTTT"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("remote sample = %v, want %v", got, want)
	}

	requests := transport.requests()
	if len(requests) == 0 {
		t.Fatal("remote read made no range requests")
	}
	for _, requestRange := range requests {
		if !strings.HasPrefix(requestRange, "bytes=") {
			t.Fatalf("request without a byte range: %q", requestRange)
		}
	}

	requestCount := len(requests)
	if err := a.IterateSample(agc.Sample{Name: "b"}, func(agc.Contig) error { return nil }); err != nil {
		t.Fatalf("second IterateSample() error = %v", err)
	}
	if got := len(transport.requests()); got != requestCount {
		t.Fatalf("cached second sample read made %d additional HTTP requests", got-requestCount)
	}
}

func TestOpenURL_GivenSmallArchive_WhenOpened_ThenDownloadsWholeObjectOnce(t *testing.T) {
	data := toyArchive(t)
	transport := &rangeTransport{data: data}
	client := &http.Client{Transport: transport}

	a, err := agc.OpenURL(context.Background(), "https://example.invalid/genomes.agc", client)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.IterateSample(agc.Sample{Name: "b"}, func(agc.Contig) error { return nil }); err != nil {
		t.Fatal(err)
	}
	requests := transport.requests()
	want := []string{"bytes=0-0", fmt.Sprintf("bytes=0-%d", len(data)-1)}
	if fmt.Sprint(requests) != fmt.Sprint(want) {
		t.Fatalf("small archive requests = %v, want %v", requests, want)
	}
}

func TestOpenURLWithOptions_GivenInvalidRemoteLimits_WhenOpened_ThenRejectsThem(t *testing.T) {
	data := toyArchive(t)
	client := &http.Client{Transport: &rangeTransport{data: data}}
	tests := []agc.HTTPOptions{
		{Client: client, Concurrency: -1},
		{Client: client, MergeGapBytes: -1},
		{Client: client, WholeObjectFraction: 1.1},
	}
	for _, options := range tests {
		if _, err := agc.OpenURLWithOptions(context.Background(), "https://example.invalid/genomes.agc", options); err == nil {
			t.Fatalf("OpenURLWithOptions() accepted invalid options %+v", options)
		}
	}
}

func TestOpenURL_GivenServerWithoutRangeSupport_WhenOpened_ThenRejectsIt(t *testing.T) {
	data := toyArchive(t)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        http.Header{"Content-Length": []string{strconv.Itoa(len(data))}},
			Body:          io.NopCloser(bytes.NewReader(data)),
			ContentLength: int64(len(data)),
		}, nil
	})}

	if _, err := agc.OpenURL(context.Background(), "https://example.invalid/genomes.agc", client); err == nil {
		t.Fatal("OpenURL() accepted a server that ignored Range")
	}
}

func TestOpenURL_GivenTruncatedRangeResponse_WhenOpened_ThenRejectsIt(t *testing.T) {
	data := toyArchive(t)
	var requests int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			return nil, err
		}
		var body []byte
		if requests == 1 {
			body = data[start : end+1]
		}
		return partialResponse(start, end, len(data), body), nil
	})}

	if _, err := agc.OpenURL(context.Background(), "https://example.invalid/genomes.agc", client); err == nil {
		t.Fatal("OpenURL() accepted a truncated range response")
	}
}

type rangeTransport struct {
	data   []byte
	mu     sync.Mutex
	ranges []string
}

func (h *rangeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	requestRange := r.Header.Get("Range")
	h.mu.Lock()
	h.ranges = append(h.ranges, requestRange)
	h.mu.Unlock()
	if requestRange != "bytes=0-0" && r.Header.Get("If-Match") != `"fixture-v1"` {
		return &http.Response{StatusCode: http.StatusPreconditionFailed, Status: "412 Precondition Failed", Header: make(http.Header), Body: http.NoBody}, nil
	}
	if encoding := r.Header.Get("Accept-Encoding"); encoding != "identity" {
		return &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Header: make(http.Header), Body: http.NoBody}, nil
	}
	var start, end int
	if _, err := fmt.Sscanf(requestRange, "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= len(h.data) {
		return &http.Response{StatusCode: http.StatusRequestedRangeNotSatisfiable, Status: "416 Range Not Satisfiable", Header: make(http.Header), Body: http.NoBody}, nil
	}
	response := partialResponse(start, end, len(h.data), h.data[start:end+1])
	response.Header.Set("ETag", `"fixture-v1"`)
	return response, nil
}

func (h *rangeTransport) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ranges...)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func partialResponse(start, end, size int, body []byte) *http.Response {
	header := make(http.Header)
	header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	header.Set("Content-Length", strconv.Itoa(end-start+1))
	return &http.Response{
		StatusCode:    http.StatusPartialContent,
		Status:        "206 Partial Content",
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(end - start + 1),
	}
}

func BenchmarkOpenURLAndReadSampleToyArchive(b *testing.B) {
	data := toyArchive(b)
	client := &http.Client{Transport: &rangeTransport{data: data}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a, err := agc.OpenURLWithOptions(context.Background(), "https://example.invalid/genomes.agc", rangedHTTPOptions(client))
		if err != nil {
			b.Fatal(err)
		}
		err = a.IterateSample(agc.Sample{Name: "b"}, func(agc.Contig) error { return nil })
		if err != nil {
			b.Fatal(err)
		}
		if err := a.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func rangedHTTPOptions(client *http.Client) agc.HTTPOptions {
	return agc.HTTPOptions{
		Client:               client,
		WholeObjectThreshold: -1,
		WholeObjectFraction:  -1,
	}
}
