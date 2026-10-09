package apiv2

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	apihandlers "github.com/Silo-Server/silo-server/internal/api/handlers"
	"github.com/Silo-Server/silo-server/internal/playback"
)

const (
	playbackAccountToken  = "token"
	playbackContentLength = "Content-Length"
	playbackLastModified  = "Last-Modified"
	playbackIntegerFormat = "int64"

	playbackMediaBinary        = "application/octet-stream"
	playbackSegmentOperation   = "getPlaybackSegment"
	playbackSubtitleOperation  = "getPlaybackSubtitle"
	playbackSubtitleHead       = "headPlaybackSubtitle"
	playbackParamTrack         = "track"
	playbackParamFileID        = "file_id"
	playbackParamPosition      = "position"
	playbackTag                = "playback"
	playbackSubtitleSubrip     = "application/x-subrip"
	playbackSubtitleVTT        = "text/vtt"
	playbackSubtitleSSA        = "text/x-ssa"
	playbackParamPath          = "path"
	playbackParamQuery         = "query"
	playbackSegmentName        = "name"
	playbackBinaryFormat       = "binary"
	playbackCacheControlHeader = "Cache-Control"
	playbackContentEncoding    = "Content-Encoding"
)

// PlaybackMediaHandlers shares the raw byte delivery protocols and the typed
// font service. Token-carried reconstruction and deny markers live in these
// shared operations; the v2 listener owns JSON envelopes and problem responses.
type PlaybackMediaHandlers struct {
	Original      http.Handler
	Manifest      http.Handler
	Segment       http.Handler
	Subtitle      http.Handler
	SubtitleFonts SubtitleFontService
}

type SubtitleFontService interface {
	SubtitleFonts(context.Context, apihandlers.SubtitleFontRequest) ([]playback.SubtitleFontBundleItem, bool, error)
}

type PlaybackSubtitleFont struct {
	Name string `json:"name" doc:"Attachment file name as authored in the container"`
	Data string `json:"data" doc:"Base64-encoded font bytes"`
}
type PlaybackSubtitleFontsInput struct {
	SessionID           ID     `path:"session_id" minLength:"1"`
	Track               string `path:"track" minLength:"1" doc:"Combined subtitle ordinal from the plan inventory"`
	FileID              string `query:"file_id" doc:"Source media file the inventory URL names; must be the plan's effective or requested file"`
	EmbeddedStreamIndex string `query:"embedded_stream_index" doc:"Stable embedded subtitle stream index from the issued inventory URL; resolves the track independently of its combined ordinal"`
	Reference           string `query:"st" doc:"Signed stream reference the plan's font bundle URL carries; account authentication and viewer authorization are always required"`
	Token               string `query:"token" doc:"Media-element fallback for the account bearer token"`
	query               url.Values
}

func (in *PlaybackSubtitleFontsInput) Resolve(ctx huma.Context) []error {
	r, _ := humachi.Unwrap(ctx)
	in.query = r.URL.Query()
	return nil
}

type PlaybackSubtitleFontsOutput struct {
	CacheControl string `header:"Cache-Control"`
	// FontBundlePending marks an in-flight extraction: the body is a valid
	// empty bundle the client must not retain as a definitive font-less
	// result. Empty on definitive answers. The header name matches the bridge
	// API's pending marker so one client constant covers both surfaces.
	FontBundlePending string `header:"X-Vio-Font-Bundle-Pending"`
	Body              Collection[PlaybackSubtitleFont]
}

func registerPlaybackDelivery(reg *Registry) {
	var handlers PlaybackMediaHandlers
	if reg.deps.PlaybackMedia != nil {
		handlers = *reg.deps.PlaybackMedia
	}
	for _, route := range []struct {
		method, path, id, protocol string
		handler                    http.Handler
		media                      []string
		ranges                     bool
	}{
		{http.MethodGet, "/stream/{session_id}", "getPlaybackMedia", "media-bytes", handlers.Original, append(playback.MediaMIMETypes(), playbackMediaBinary, "multipart/byteranges"), true},
		{http.MethodHead, "/stream/{session_id}", "headPlaybackMedia", "media-bytes", handlers.Original, nil, true},
		{http.MethodGet, "/playback/transcode/{session_id}/master.m3u8", "getPlaybackManifest", "hls", handlers.Manifest, []string{"application/vnd.apple.mpegurl"}, false},
		{http.MethodGet, "/playback/transcode/{session_id}/segment/{name}", playbackSegmentOperation, "hls", handlers.Segment, []string{"video/mp4", "video/mp2t", playbackMediaBinary, "multipart/byteranges"}, true},
		{http.MethodGet, "/stream/{session_id}/subtitles/{track}", playbackSubtitleOperation, "subtitle-sidecar", handlers.Subtitle, []string{playbackSubtitleVTT, playbackSubtitleSSA, playbackSubtitleSubrip, playbackMediaBinary}, false},
		{http.MethodHead, "/stream/{session_id}/subtitles/{track}", playbackSubtitleHead, "subtitle-sidecar", handlers.Subtitle, nil, false},
	} {
		params := []*huma.Param{
			{Name: "session_id", In: playbackParamPath, Required: true, Schema: &huma.Schema{Type: huma.TypeString, MinLength: new(1)}},
			{Name: playbackAccountToken, In: playbackParamQuery, Description: "Media-element fallback for the account bearer token when an Authorization header cannot be set. Header-authenticated media requires the Authorization header and the profile selector.", Schema: &huma.Schema{Type: huma.TypeString}},
			{Name: "st", In: playbackParamQuery, Description: "Signed stream reference the plan URL carries; it reconstructs the session after a restart. Omitted for header-authenticated media. Account and viewer authorization are always required.", Schema: &huma.Schema{Type: huma.TypeString}},
		}
		if route.id == playbackSegmentOperation {
			params = append(params, &huma.Param{Name: playbackSegmentName, In: playbackParamPath, Required: true, Schema: &huma.Schema{Type: huma.TypeString, MinLength: new(1)}})
		}
		subtitle := route.id == playbackSubtitleOperation || route.id == playbackSubtitleHead
		if subtitle {
			params = append(params,
				&huma.Param{Name: playbackParamTrack, In: playbackParamPath, Required: true, Description: "Combined subtitle ordinal from the plan inventory, optionally suffixed with the sidecar extension (.vtt, .ass, .sup, .srt) the inventory URL carries.", Schema: &huma.Schema{Type: huma.TypeString, MinLength: new(1)}},
				&huma.Param{Name: playbackParamFileID, In: playbackParamQuery, Description: "Source media file the inventory URL names; must be the plan's effective or requested file.", Schema: &huma.Schema{Type: huma.TypeString}},
				&huma.Param{Name: playback.DownloadedSubtitleIDParamV3, In: playbackParamQuery, Description: "Stable downloaded-subtitle identity the inventory URL carries; must belong to the source file.", Schema: &huma.Schema{Type: huma.TypeString}},
				&huma.Param{Name: playback.SubtitleOriginalParamV3, In: playbackParamQuery, Description: "1 on a .srt URL published under subrip_sidecar_v1: serve the stored SRT bytes instead of the WebVTT conversion.", Schema: &huma.Schema{Type: huma.TypeString}},
				&huma.Param{Name: playbackParamPosition, In: playbackParamQuery, Description: "Seek position in seconds for windowed text extraction.", Schema: &huma.Schema{Type: huma.TypeNumber}},
				&huma.Param{Name: "duration", In: playbackParamQuery, Description: "Window length in seconds for text extraction.", Schema: &huma.Schema{Type: huma.TypeNumber}},
				&huma.Param{Name: "windowed", In: playbackParamQuery, Description: "PGS: opt into a positioned window instead of the whole track.", Schema: &huma.Schema{Type: huma.TypeString}})
		}
		content := map[string]*huma.MediaType{}
		for _, media := range route.media {
			content[media] = &huma.MediaType{Schema: &huma.Schema{Type: huma.TypeString, Format: playbackBinaryFormat}}
		}
		headers := map[string]*huma.Param{playbackContentLength: {Schema: &huma.Schema{Type: huma.TypeInteger, Format: playbackIntegerFormat}}, playbackCacheControlHeader: {Schema: &huma.Schema{Type: huma.TypeString}}}
		responses := map[string]*huma.Response{"200": {Description: "Playback bytes or HEAD metadata", Content: content, Headers: headers}}
		if route.ranges {
			headers["Accept-Ranges"] = &huma.Param{Schema: &huma.Schema{Type: huma.TypeString}}
			headers[etagField] = &huma.Param{Schema: &huma.Schema{Type: huma.TypeString}}
			headers[playbackLastModified] = &huma.Param{Schema: &huma.Schema{Type: huma.TypeString}}
			responses["206"] = &huma.Response{Description: "Requested byte range", Content: content, Headers: map[string]*huma.Param{"Content-Range": {Schema: &huma.Schema{Type: huma.TypeString}}}}
			responses["304"] = &huma.Response{Description: "The authorized representation has not changed"}
			params = append(params, &huma.Param{Name: ifMatchField, In: paramInHeader, Schema: &huma.Schema{Type: huma.TypeString}}, &huma.Param{Name: "If-Unmodified-Since", In: paramInHeader, Schema: &huma.Schema{Type: huma.TypeString}}, &huma.Param{Name: "Range", In: paramInHeader, Schema: &huma.Schema{Type: huma.TypeString}}, &huma.Param{Name: "If-Range", In: paramInHeader, Schema: &huma.Schema{Type: huma.TypeString}}, &huma.Param{Name: ifNoneMatchField, In: paramInHeader, Schema: &huma.Schema{Type: huma.TypeString}}, &huma.Param{Name: "If-Modified-Since", In: paramInHeader, Schema: &huma.Schema{Type: huma.TypeString}})
		}
		// 503 covers a v1 handler's own 503 dependency failure and its
		// retryable 502 upstream failures, which playbackDeliveryProblemType
		// remaps onto dependency_unavailable.
		statuses := []int{400, 404, 409, 410, 422, 500, 503}
		if route.ranges {
			statuses = append(statuses, 412, 416)
		}
		if subtitle {
			statuses = append(statuses, 415)
		}
		for _, status := range statuses {
			responses[strconv.Itoa(status)] = &huma.Response{Description: http.StatusText(status), Content: map[string]*huma.MediaType{problemContentType: {Schema: reg.api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[Problem](), true, "")}}}
		}
		if route.ranges {
			responses["416"].Headers = map[string]*huma.Param{"Content-Range": {Schema: &huma.Schema{Type: huma.TypeString}}}
		}
		reason := "Token-authorized media retains native byte, range, HEAD and HLS semantics without JSON buffering."
		if subtitle {
			reason = "Token-authorized sidecar text and bitmap bytes are streamed as extracted."
		}
		raw := RawOperation{Operation: Operation{Operation: huma.Operation{Method: route.method, Path: Prefix + route.path, OperationID: route.id, Tags: []string{playbackTag}, Parameters: params, Responses: responses}, Class: ClassProfileScoped, ProfileOptional: true, ServiceBacked: true}, Protocol: route.protocol, Reason: reason}
		RegisterRaw(reg, raw, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !playbackUUID(chi.URLParam(r, "session_id")) {
				writeProblem(w, r, validationProblem("path.session_id", "invalid", "Expected a canonical UUID."))
				return
			}
			if route.handler == nil {
				writeProblem(w, r, NewProblem(TypeDependencyUnavailable, "Playback delivery is not configured."))
				return
			}
			route.handler.ServeHTTP(&playbackDeliveryWriter{ResponseWriter: w, request: r}, r)
		}))
	}
	fonts := humaOp(http.MethodGet, Prefix+"/stream/{session_id}/subtitles/{track}/fonts", "getPlaybackSubtitleFonts", playbackTag,
		"Read the attached-font bundle of a session's embedded ASS/SSA subtitle track. Admission is the sidecar's: account authentication, viewer authorization and the session the plan named.")
	fonts.Errors = []int{http.StatusNotFound, http.StatusGone}
	Register(reg, Operation{Operation: fonts, Class: ClassProfileScoped, ProfileOptional: true, ServiceBacked: true}, func(ctx context.Context, in *PlaybackSubtitleFontsInput) (*PlaybackSubtitleFontsOutput, error) {
		if !playbackUUID(string(in.SessionID)) {
			return nil, validationProblem("path.session_id", "invalid", "Expected a canonical UUID.")
		}
		if reg.deps.PlaybackMedia == nil || reg.deps.PlaybackMedia.SubtitleFonts == nil {
			return nil, NewProblem(TypeDependencyUnavailable, "Playback delivery is not configured.")
		}
		items, pending, err := reg.deps.PlaybackMedia.SubtitleFonts.SubtitleFonts(ctx, apihandlers.SubtitleFontRequest{
			SessionID: string(in.SessionID), Track: in.Track, Query: in.query,
		})
		if err != nil {
			return nil, playbackSubtitleFontProblem(err)
		}
		fonts := make([]PlaybackSubtitleFont, 0, len(items))
		for _, item := range items {
			fonts = append(fonts, PlaybackSubtitleFont{Name: item.Name, Data: item.Data})
		}
		out := &PlaybackSubtitleFontsOutput{Body: NewCollection(fonts)}
		if pending {
			// In-flight extraction: uncacheable empty bundle plus the pending
			// marker, mirroring the bridge API. The client falls back to
			// default fonts immediately and re-fetches for the completed
			// bundle instead of retaining the empty state.
			out.CacheControl = "no-store"
			out.FontBundlePending = "true"
		} else {
			// Definitive answer (fonts or a genuinely font-less file):
			// cacheable, so the client stops after one fetch instead of
			// treating every response as pending.
			out.CacheControl = "private, max-age=600"
		}
		return out, nil
	})
}

func playbackSubtitleFontProblem(err error) *Problem {
	failure, ok := errors.AsType[*apihandlers.APIError](err)
	if !ok || failure.Status >= 500 && failure.Status != http.StatusServiceUnavailable {
		return NewProblem(TypeInternalError, "An unexpected error occurred.")
	}
	kind := playbackProblemType(failure.Status, failure.Code)
	if failure.Status == http.StatusGone {
		kind = TypePlaybackSessionEnded
	}
	detail := failure.Message
	if detail == "" {
		detail = kind.Title
	}
	return NewProblem(kind, detail)
}

// playbackDeliveryWriter carries the v1 handler's machine-readable error code
// to the v2 delivery adapter so a semantic problem (e.g. a retryable
// provider_unavailable) survives the status-only adaptation. The v1 handler
// writes its code into the response body, which the adapter discards before
// commitment, so the code is captured out of band.
type playbackDeliveryWriter struct {
	http.ResponseWriter
	request      *http.Request
	inner        *streamResponseWriter
	problemCode  string
	problemCause error
}

// SetPlaybackProblemCode records the v1 error code for the next pre-body
// WriteHeader. It is a no-op after the response is committed.
func (w *playbackDeliveryWriter) SetPlaybackProblemCode(code string) {
	if w.problemCode == "" {
		w.problemCode = code
	}
}

// SetPlaybackProblemCause records the underlying stream failure behind the v1
// handler's generic error body, so the request log and the discarded-body log
// name it. Like SetPlaybackProblemCode the cause is carried out of band and
// never reaches the response body. It is a no-op after the response is
// committed.
func (w *playbackDeliveryWriter) SetPlaybackProblemCause(err error) {
	if w.problemCause == nil {
		w.problemCause = err
	}
}

func (w *playbackDeliveryWriter) transport() *streamResponseWriter {
	if w.inner == nil {
		w.inner = &streamResponseWriter{ResponseWriter: w.ResponseWriter, request: w.request, problemType: func(status int) ProblemType {
			return playbackDeliveryProblemType(status, w.problemCode)
		}, redactHeaders: []string{playbackContentLength, playbackContentEncoding, directDisposition, jobLocationHeader, etagField, playbackLastModified}, cause: w.problemCause}
	}
	return w.inner
}

// playbackDeliveryProblemType maps a pre-body failure status of a v1 media
// handler onto the catalog. A 410 is the stream deny marker (the session was
// stopped or expired), which has its own corrective action: start again. A 502
// is a retryable upstream/dependency failure (a virtual source resolve, relay,
// or transcode node), not a server defect: the catalog represents temporary
// dependency unavailability as 503 dependency_unavailable, so map it there
// rather than letting the catalog's internal_error default report a server bug.
// The v2 route declares 503 for exactly this reason.
//
// A v1 handler may also write a 503 with a machine-readable code that deserves
// its own v2 type: provider_unavailable distinguishes a transient provider
// outage (keep retrying this release) from a missing server dependency. The
// code is matched only at 503 so a mismatched handler can never mint a type
// whose status disagrees with the response.
func playbackDeliveryProblemType(status int, code string) ProblemType {
	switch status {
	case http.StatusGone:
		return TypePlaybackSessionEnded
	case http.StatusBadGateway:
		return TypeDependencyUnavailable
	case http.StatusServiceUnavailable:
		if code == TypeProviderUnavailable.ID {
			return TypeProviderUnavailable
		}
	}
	return TypeForStatus(status)
}
func (w *playbackDeliveryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *playbackDeliveryWriter) WriteHeader(status int) {
	// A raw byte handler writes its own error envelope and discards the
	// detail; the v2 request log would otherwise record only a bare 500. The
	// v1 machine-readable code was captured by SetPlaybackProblemCode, so
	// record it (with the request ID already on the context) before the
	// adapter collapses the body into a problem envelope.
	if status >= http.StatusInternalServerError {
		code := w.problemCode
		if code == "" {
			code = TypeInternalError.ID
		}
		message := fmt.Sprintf("playback delivery failed: status=%d code=%s", status, code)
		if w.problemCause != nil {
			message = fmt.Sprintf("%s: %v", message, w.problemCause)
		}
		noteOperationError(w.request.Context(), errors.New(message))
	}
	w.transport().WriteHeader(status)
}

func (w *playbackDeliveryWriter) Write(data []byte) (int, error) { return w.transport().Write(data) }
func (w *playbackDeliveryWriter) FlushError() error              { return w.transport().FlushError() }
