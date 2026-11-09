package keyframes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ErrTransient marks a keyframe read that failed for a reason that says
// nothing lasting about the file: a network error, a timeout, or an upstream
// status. Callers must not record a planning decision from it, so a later
// playback can retry the same source.
var ErrTransient = errors.New("keyframes: transient source read failure")

const (
	// httpRangeReadBudget bounds one remote index read. The quick Matroska
	// scan reads a handful of metadata elements; a source that needs more
	// than this is not going to plan segments quickly enough for a playback
	// start.
	httpRangeReadBytes = 32 << 20
	httpRangeReadCount = 512
)

// ReadHTTP reads a remote Matroska file's keyframe index through HTTP Range
// requests, without downloading the media. It runs the quick check only: the
// cluster-coverage sample is skipped because it costs one request per block
// header, and a remote relay bills every range. A source whose container
// index is incomplete therefore plans from the cues it declares; the segment
// recovery path re-probes any segment that turns out not to line up.
//
// The returned Index carries Duration but never VideoEnd, because only the
// full cluster verification knows where the video track ends. Callers that
// need a last-segment end fall back to Duration.
func ReadHTTP(ctx context.Context, client *http.Client, source string) (Index, error) {
	if client == nil {
		client = http.DefaultClient
	}
	size, err := httpSourceSize(ctx, client, source)
	if err != nil {
		return Index{}, err
	}
	if size <= 0 {
		return Index{}, fmt.Errorf("%w: empty source", ErrTransient)
	}
	r := &httpRangeReaderAt{
		ctx:      ctx,
		client:   client,
		source:   source,
		size:     size,
		maxBytes: httpRangeReadBytes,
		maxReads: httpRangeReadCount,
	}
	m, err := readMatroskaWithCoverage(r, size, false)
	if err != nil {
		return Index{}, settleHTTPIndexError(err)
	}
	return m.Index, nil
}

// settleHTTPIndexError keeps a stable "no usable index" verdict distinct from
// a read failure the caller may retry.
func settleHTTPIndexError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNoIndex) {
		return err
	}
	var notMatroska notMatroskaError
	if errors.As(err, &notMatroska) {
		return fmt.Errorf("%w: %w", ErrNoIndex, err)
	}
	return err
}

// httpSourceSize reports the source's length without reading its body. A HEAD
// is preferred; a Range request that asks for the first byte covers origins
// (and relays) that answer HEAD with 405/501 or no length.
func httpSourceSize(ctx context.Context, client *http.Client, source string) (int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, source, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrTransient, err)
	}
	response, err := client.Do(request)
	if err == nil {
		_ = response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if length, parseErr := strconv.ParseInt(strings.TrimSpace(response.Header.Get("Content-Length")), 10, 64); parseErr == nil && length > 0 {
				return length, nil
			}
		}
	}
	return httpRangeSourceSize(ctx, client, source)
}

// httpRangeSourceSize asks for the first byte and reads the total from
// Content-Range, or Content-Length when the origin ignored the Range.
func httpRangeSourceSize(ctx context.Context, client *http.Client, source string) (int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrTransient, err)
	}
	request.Header.Set("Range", "bytes=0-0")
	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrTransient, err)
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusPartialContent:
		if total := totalFromContentRange(response.Header.Get("Content-Range")); total > 0 {
			return total, nil
		}
	case http.StatusOK:
		if length, parseErr := strconv.ParseInt(strings.TrimSpace(response.Header.Get("Content-Length")), 10, 64); parseErr == nil && length > 0 {
			return length, nil
		}
	}
	return 0, fmt.Errorf("%w: source length unavailable (HTTP %d)", ErrTransient, response.StatusCode)
}

// totalFromContentRange parses "bytes 0-0/12345" and returns 12345.
func totalFromContentRange(value string) int64 {
	_, total, ok := strings.Cut(strings.TrimSpace(value), "/")
	if !ok {
		return 0
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// httpRangeReaderAt is an io.ReaderAt over one remote source. Each read is one
// bounded Range request; the Matroska metadata reader asks for small elements,
// so the whole index costs a handful of requests rather than the file.
type httpRangeReaderAt struct {
	// io.ReaderAt takes no context, so the reader carries the bounded read's
	// context to cancel its range requests with it.
	ctx      context.Context //nolint:containedctx // see above
	client   *http.Client
	source   string
	size     int64
	maxBytes int64
	maxReads int
	read     int64
	reads    int
}

func (r *httpRangeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("keyframes: negative offset %d", off)
	}
	if off >= r.size {
		return 0, io.EOF
	}
	end := off + int64(len(p)) - 1
	if end >= r.size {
		end = r.size - 1
	}
	want := end - off + 1
	if r.reads >= r.maxReads || r.read+want > r.maxBytes {
		return 0, fmt.Errorf("%w: remote index read budget exceeded", ErrTransient)
	}
	r.reads++
	r.read += want

	request, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.source, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrTransient, err)
	}
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	response, err := r.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrTransient, err)
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusPartialContent:
		return readRangeBody(response.Body, p)
	case http.StatusOK:
		// An origin that ignores Range streams the file from zero. Discard
		// the prefix and take only the requested window.
		if off > 0 {
			if _, err := io.CopyN(io.Discard, response.Body, off); err != nil {
				return 0, fmt.Errorf("%w: %w", ErrTransient, err)
			}
		}
		return readRangeBody(response.Body, p)
	case http.StatusRequestedRangeNotSatisfiable:
		return 0, io.EOF
	default:
		return 0, fmt.Errorf("%w: source answered HTTP %d", ErrTransient, response.StatusCode)
	}
}

// readRangeBody fills p from a response body. A short body is EOF, which the
// Matroska reader treats as a truncated element.
func readRangeBody(body io.Reader, p []byte) (int, error) {
	n, err := io.ReadFull(body, p)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return n, io.EOF
		}
		return n, fmt.Errorf("%w: %w", ErrTransient, err)
	}
	return n, nil
}
