package agc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	defaultRemoteReadAhead      = int64(64 << 10)
	defaultRemoteMergeGap       = int64(64 << 10)
	defaultRemoteMaxMergedRange = int64(8 << 20)
	defaultRemoteMaxCacheBytes  = int64(64 << 20)
	defaultRemoteConcurrency    = 8
	defaultWholeObjectThreshold = int64(16 << 20)
	defaultWholeObjectFraction  = 0.80
)

// HTTPOptions controls remote archive retrieval. Zero-valued fields select
// defaults. Set WholeObjectThreshold or WholeObjectFraction to a negative
// value to disable that whole-object optimization.
type HTTPOptions struct {
	// Client performs HTTP requests. A nil client uses http.DefaultClient.
	Client *http.Client
	// ReadAheadBytes is fetched for small demand reads. The default is 64 KiB.
	ReadAheadBytes int64
	// MergeGapBytes is the largest gap included while joining nearby ranges.
	// The default is 64 KiB.
	MergeGapBytes int64
	// MaxMergedRangeBytes caps one joined range. The default is 8 MiB.
	MaxMergedRangeBytes int64
	// CacheBytes bounds compressed remote range data. The default is 64 MiB.
	CacheBytes int64
	// Concurrency caps simultaneous range requests. The default is 8.
	Concurrency int
	// WholeObjectThreshold downloads objects at or below this size in one
	// full-range request after the range-support probe. The default is 16 MiB;
	// a negative value disables it.
	WholeObjectThreshold int64
	// WholeObjectFraction downloads the whole object when uncached planned
	// ranges reach this fraction and the object fits CacheBytes. The default is
	// 0.80; a negative value disables it.
	WholeObjectFraction float64
}

type resolvedHTTPOptions struct {
	client               *http.Client
	readAhead            int64
	mergeGap             int64
	maxMergedRange       int64
	cacheBytes           int64
	concurrency          int
	wholeObjectThreshold int64
	wholeObjectFraction  float64
}

// OpenURL opens an AGC v3 archive through HTTP byte-range requests. The
// context remains in force for the lifetime of the archive. A nil client uses
// http.DefaultClient. The server must support single byte ranges.
func OpenURL(ctx context.Context, rawURL string, client *http.Client) (*Archive, error) {
	return OpenURLWithOptions(ctx, rawURL, HTTPOptions{Client: client})
}

// OpenURLWithOptions opens an HTTP byte-range-backed AGC v3 archive using the
// supplied retrieval policy.
func OpenURLWithOptions(ctx context.Context, rawURL string, options HTTPOptions) (*Archive, error) {
	if ctx == nil {
		return nil, fmt.Errorf("agc: nil context")
	}
	resolved, err := resolveHTTPOptions(options)
	if err != nil {
		return nil, err
	}
	r, err := newHTTPReaderAt(ctx, resolved, rawURL)
	if err != nil {
		return nil, err
	}
	if resolved.wholeObjectThreshold >= 0 && r.size <= resolved.wholeObjectThreshold {
		data, err := r.fetch(readRange{offset: 0, length: r.size})
		_ = r.Close()
		if err != nil {
			return nil, fmt.Errorf("agc: fetch complete remote archive: %w", err)
		}
		a, err := openReaderAt(bytes.NewReader(data), int64(len(data)), nil)
		if err != nil {
			return nil, fmt.Errorf("agc: open %q: %w", rawURL, err)
		}
		return a, nil
	}
	a, err := openReaderAt(r, r.size, r)
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("agc: open %q: %w", rawURL, err)
	}
	return a, nil
}

func resolveHTTPOptions(options HTTPOptions) (resolvedHTTPOptions, error) {
	resolved := resolvedHTTPOptions{
		client: options.Client, readAhead: defaultRemoteReadAhead,
		mergeGap: defaultRemoteMergeGap, maxMergedRange: defaultRemoteMaxMergedRange,
		cacheBytes: defaultRemoteMaxCacheBytes, concurrency: defaultRemoteConcurrency,
		wholeObjectThreshold: defaultWholeObjectThreshold, wholeObjectFraction: defaultWholeObjectFraction,
	}
	if resolved.client == nil {
		resolved.client = http.DefaultClient
	}
	if options.ReadAheadBytes < 0 || options.MergeGapBytes < 0 || options.MaxMergedRangeBytes < 0 || options.CacheBytes < 0 || options.Concurrency < 0 {
		return resolvedHTTPOptions{}, fmt.Errorf("agc: HTTP byte and concurrency limits cannot be negative")
	}
	if options.ReadAheadBytes > 0 {
		resolved.readAhead = options.ReadAheadBytes
	}
	if options.MergeGapBytes > 0 {
		resolved.mergeGap = options.MergeGapBytes
	}
	if options.MaxMergedRangeBytes > 0 {
		resolved.maxMergedRange = options.MaxMergedRangeBytes
	}
	if options.CacheBytes > 0 {
		resolved.cacheBytes = options.CacheBytes
	}
	if options.Concurrency > 0 {
		resolved.concurrency = options.Concurrency
	}
	if options.WholeObjectThreshold < 0 {
		resolved.wholeObjectThreshold = -1
	} else if options.WholeObjectThreshold > 0 {
		resolved.wholeObjectThreshold = options.WholeObjectThreshold
	}
	if options.WholeObjectFraction < 0 {
		resolved.wholeObjectFraction = -1
	} else if options.WholeObjectFraction > 1 {
		return resolvedHTTPOptions{}, fmt.Errorf("agc: WholeObjectFraction must be between 0 and 1")
	} else if options.WholeObjectFraction > 0 {
		resolved.wholeObjectFraction = options.WholeObjectFraction
	}
	if resolved.maxMergedRange < resolved.readAhead {
		return resolvedHTTPOptions{}, fmt.Errorf("agc: MaxMergedRangeBytes must be at least ReadAheadBytes")
	}
	return resolved, nil
}

type readRange struct {
	offset int64
	length int64
}

func (r readRange) end() int64 { return r.offset + r.length }

type cachedReadRange struct {
	readRange
	data []byte
}

// rangePrefetcher is deliberately internal: local files and arbitrary
// ReaderAt implementations keep their existing demand-driven behaviour.
type rangePrefetcher interface {
	Prefetch([]readRange) error
}

type httpReaderAt struct {
	ctx     context.Context
	client  *http.Client
	url     string
	size    int64
	etag    string
	options resolvedHTTPOptions

	mu         sync.Mutex
	closed     bool
	cache      []cachedReadRange
	cacheBytes int64
}

func newHTTPReaderAt(ctx context.Context, options resolvedHTTPOptions, rawURL string) (*httpReaderAt, error) {
	r := &httpReaderAt{ctx: ctx, client: options.client, url: rawURL, options: options}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("agc: create remote probe: %w", err)
	}
	request.Header.Set("Range", "bytes=0-0")
	request.Header.Set("Accept-Encoding", "identity")
	response, err := options.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("agc: probe remote archive: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("agc: remote server does not support byte ranges: got HTTP %s", response.Status)
	}
	start, end, size, err := parseContentRange(response.Header.Get("Content-Range"))
	if err != nil || start != 0 || end != 0 || size <= 0 {
		return nil, fmt.Errorf("agc: invalid remote Content-Range %q", response.Header.Get("Content-Range"))
	}
	var one [1]byte
	if _, err := io.ReadFull(response.Body, one[:]); err != nil {
		return nil, fmt.Errorf("agc: read remote probe: %w", err)
	}
	r.size = size
	if etag := response.Header.Get("ETag"); etag != "" && !strings.HasPrefix(strings.TrimSpace(etag), "W/") {
		r.etag = etag
	}
	return r, nil
}

func parseContentRange(value string) (start, end, size int64, err error) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, errors.New("missing bytes prefix")
	}
	value = strings.TrimPrefix(value, "bytes ")
	span, total, ok := strings.Cut(value, "/")
	if !ok {
		return 0, 0, 0, errors.New("missing total size")
	}
	first, last, ok := strings.Cut(span, "-")
	if !ok {
		return 0, 0, 0, errors.New("missing range end")
	}
	start, err = strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, 0, 0, err
	}
	end, err = strconv.ParseInt(last, 10, 64)
	if err != nil {
		return 0, 0, 0, err
	}
	size, err = strconv.ParseInt(total, 10, 64)
	if err != nil || start < 0 || end < start || size <= end {
		return 0, 0, 0, errors.New("invalid range values")
	}
	return start, end, size, nil
}

func (r *httpReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("agc: negative remote read offset %d", off)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, ErrClosed
	}
	if data, ok := r.cachedLocked(off, int64(len(p))); ok {
		copy(p, data)
		r.mu.Unlock()
		return len(p), nil
	}
	r.mu.Unlock()
	if off >= r.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	short := false
	if want > r.size-off {
		want = r.size - off
		short = true
	}
	fetchLength := want
	if fetchLength < r.options.readAhead {
		fetchLength = min(r.options.readAhead, r.size-off)
	}
	data, err := r.fetch(readRange{offset: off, length: fetchLength})
	if err != nil {
		return 0, err
	}
	r.addCache(cachedReadRange{readRange: readRange{offset: off, length: int64(len(data))}, data: data})
	n := copy(p, data[:want])
	if short {
		return n, io.EOF
	}
	return n, nil
}

func (r *httpReaderAt) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.cache = nil
	r.cacheBytes = 0
	return nil
}

func (r *httpReaderAt) Prefetch(ranges []readRange) error {
	merged := mergeReadRanges(ranges, r.options.mergeGap, r.options.maxMergedRange)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	uncached := merged[:0]
	for _, requestRange := range merged {
		if _, ok := r.cachedLocked(requestRange.offset, requestRange.length); !ok {
			uncached = append(uncached, requestRange)
		}
	}
	r.mu.Unlock()
	merged = uncached
	if len(merged) == 0 {
		return nil
	}
	if r.options.wholeObjectFraction > 0 {
		var plannedBytes int64
		for _, requestRange := range merged {
			plannedBytes += requestRange.length
		}
		if shouldFetchWholeObject(plannedBytes, r.size, r.options.cacheBytes, r.options.wholeObjectFraction) {
			data, err := r.fetch(readRange{offset: 0, length: r.size})
			if err != nil {
				return err
			}
			r.addCache(cachedReadRange{readRange: readRange{offset: 0, length: r.size}, data: data})
			return nil
		}
	}

	type result struct {
		requestRange readRange
		data         []byte
		err          error
	}
	jobs := make(chan int)
	results := make(chan result)
	workers := min(r.options.concurrency, len(merged))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				data, err := r.fetch(merged[i])
				results <- result{requestRange: merged[i], data: data, err: err}
			}
		}()
	}
	go func() {
		for i := range merged {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var firstErr error
	for result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		r.addCache(cachedReadRange{readRange: result.requestRange, data: result.data})
	}
	if firstErr != nil {
		return firstErr
	}
	return nil
}

func shouldFetchWholeObject(plannedBytes, objectBytes, cacheBytes int64, fraction float64) bool {
	return fraction > 0 && objectBytes > 0 && objectBytes <= cacheBytes &&
		float64(plannedBytes) >= fraction*float64(objectBytes)
}

func (r *httpReaderAt) fetch(requestRange readRange) ([]byte, error) {
	if requestRange.offset < 0 || requestRange.length <= 0 || requestRange.offset > r.size-requestRange.length {
		return nil, fmt.Errorf("agc: invalid remote range offset=%d length=%d size=%d", requestRange.offset, requestRange.length, r.size)
	}
	request, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, fmt.Errorf("agc: create range request: %w", err)
	}
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", requestRange.offset, requestRange.end()-1))
	request.Header.Set("Accept-Encoding", "identity")
	if r.etag != "" {
		request.Header.Set("If-Match", r.etag)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("agc: fetch remote range: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("agc: remote range %d-%d returned HTTP %s", requestRange.offset, requestRange.end()-1, response.Status)
	}
	if responseETag := response.Header.Get("ETag"); r.etag != "" && responseETag != "" && responseETag != r.etag {
		return nil, fmt.Errorf("agc: remote object changed: ETag %q, want %q", responseETag, r.etag)
	}
	start, end, size, err := parseContentRange(response.Header.Get("Content-Range"))
	if err != nil || start != requestRange.offset || end != requestRange.end()-1 || size != r.size {
		return nil, fmt.Errorf("agc: invalid remote Content-Range %q", response.Header.Get("Content-Range"))
	}
	data := make([]byte, int(requestRange.length))
	if _, err := io.ReadFull(response.Body, data); err != nil {
		return nil, fmt.Errorf("agc: read remote range %d-%d: %w", requestRange.offset, requestRange.end()-1, err)
	}
	var extra [1]byte
	if n, err := response.Body.Read(extra[:]); n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return nil, fmt.Errorf("agc: remote range returned excess data")
	}
	return data, nil
}

func (r *httpReaderAt) cachedLocked(off, length int64) ([]byte, bool) {
	for _, entry := range r.cache {
		if off >= entry.offset && length <= entry.end()-off {
			start := off - entry.offset
			return entry.data[start : start+length], true
		}
	}
	return nil, false
}

func (r *httpReaderAt) addCache(entry cachedReadRange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if _, ok := r.cachedLocked(entry.offset, entry.length); ok {
		return
	}
	r.cache = append(r.cache, entry)
	r.cacheBytes += entry.length
	for r.cacheBytes > r.options.cacheBytes && len(r.cache) > 1 {
		r.cacheBytes -= r.cache[0].length
		r.cache = r.cache[1:]
	}
}

func mergeReadRanges(ranges []readRange, gap, maxLength int64) []readRange {
	filtered := make([]readRange, 0, len(ranges))
	for _, candidate := range ranges {
		if candidate.offset >= 0 && candidate.length > 0 {
			filtered = append(filtered, candidate)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].offset == filtered[j].offset {
			return filtered[i].length < filtered[j].length
		}
		return filtered[i].offset < filtered[j].offset
	})
	merged := make([]readRange, 0, len(filtered))
	for _, candidate := range filtered {
		if len(merged) == 0 {
			merged = append(merged, candidate)
			continue
		}
		last := &merged[len(merged)-1]
		end := max(last.end(), candidate.end())
		if candidate.offset <= last.end()+gap && end-last.offset <= maxLength {
			last.length = end - last.offset
			continue
		}
		merged = append(merged, candidate)
	}
	return merged
}
