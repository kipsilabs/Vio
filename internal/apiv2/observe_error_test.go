package apiv2

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestRequestLogCarries500Cause pins that a 500 request log names the
// underlying application error under the same request ID, for both shapes an
// operation can fail with:
//
//   - a plain (non-StatusError) error, which Huma converts to the fixed
//     internal_error envelope and would otherwise discard;
//   - a service error masked by serviceProblem, whose cause is carried on the
//     Problem for the log and never the response body.
func TestRequestLogCarries500Cause(t *testing.T) {
	buf := captureLogs(t)
	h := NewHandler(Dependencies{testRegister: func(reg *Registry) {
		Register(reg, Operation{
			Operation: humaOp(http.MethodGet, Prefix+"/probe/fail/internal", "probeFailInternal", "probe", "masked service error"),
			Class:     ClassPublic,
		}, func(context.Context, *struct{}) (*probeOutput, error) {
			return nil, serviceProblem(errors.New("boom masked cause"))
		})
		Register(reg, Operation{
			Operation: humaOp(http.MethodGet, Prefix+"/probe/fail/plain", "probeFailPlain", "probe", "plain error"),
			Class:     ClassPublic,
		}, func(context.Context, *struct{}) (*probeOutput, error) {
			return nil, errors.New("boom plain cause")
		})
		// A masked request-cancellation problem carries the cause for the
		// request log (same envelope shape as the masked service error).
		Register(reg, Operation{
			Operation: humaOp(http.MethodGet, Prefix+"/probe/fail/canceled", "probeFailCanceled", "probe", "canceled lookup"),
			Class:     ClassPublic,
		}, func(context.Context, *struct{}) (*probeOutput, error) {
			return nil, catalogProblem(fmt.Errorf("lookup private-marker-9f3: %w", context.Canceled), "query.source")
		})
		// The requests-domain mapper names the same cancellation.
		Register(reg, Operation{
			Operation: humaOp(http.MethodGet, Prefix+"/probe/fail/canceled-request", "probeFailCanceledRequest", "probe", "canceled request lookup"),
			Class:     ClassPublic,
		}, func(context.Context, *struct{}) (*probeOutput, error) {
			return nil, requestProblem(fmt.Errorf("request private-marker-51c: %w", context.Canceled))
		})
		// A watch access-filter failure masks its cause the same way.
		Register(reg, Operation{
			Operation: humaOp(http.MethodGet, Prefix+"/probe/fail/access-filter", "probeFailAccessFilter", "probe", "canceled access filter"),
			Class:     ClassPublic,
		}, func(context.Context, *struct{}) (*probeOutput, error) {
			return nil, NewProblem(TypeInternalError, "An unexpected error occurred.").withCause(fmt.Errorf("filter private-marker-77d: %w", context.Canceled))
		})
	}})

	cases := []struct {
		path         string
		want         string
		privateMark  string
		canceledBody string
	}{
		{"/api/v2/probe/fail/internal", "boom masked cause", "", ""},
		{"/api/v2/probe/fail/plain", "boom plain cause", "", ""},
		{"/api/v2/probe/fail/canceled", "context canceled", "private-marker-9f3", "The request was canceled."},
		{"/api/v2/probe/fail/canceled-request", "context canceled", "private-marker-51c", "The request was canceled."},
		{"/api/v2/probe/fail/access-filter", "context canceled", "private-marker-77d", "An unexpected error occurred."},
	}
	for _, tc := range cases {
		buf.Reset()
		rec := do(t, h, http.MethodGet, tc.path, "", nil)
		requireProblem(t, rec, TypeInternalError)
		line := buf.String()
		if !strings.Contains(line, `"status":500`) {
			t.Errorf("%s: log line does not report the 500: %s", tc.path, line)
		}
		if !strings.Contains(line, tc.want) {
			t.Errorf("%s: log line does not name the cause %q: %s", tc.path, tc.want, line)
		}
		if !strings.Contains(line, `"request_id":"`+requestIDHeader(rec)+`"`) {
			t.Errorf("%s: log line does not carry the request ID: %s", tc.path, line)
		}
		if !strings.Contains(line, `"error_code":"internal_error"`) {
			t.Errorf("%s: log line does not carry the problem code: %s", tc.path, line)
		}
		if tc.privateMark != "" {
			// The cause reaches the request log but never the response
			// body: a masked problem must not disclose its marker.
			if strings.Contains(rec.Body.String(), tc.privateMark) {
				t.Errorf("%s: response body leaks the masked marker %q: %s", tc.path, tc.privateMark, rec.Body.String())
			}
			if tc.canceledBody != "" && !strings.Contains(rec.Body.String(), tc.canceledBody) {
				t.Errorf("%s: response body does not name the cancellation: %s", tc.path, rec.Body.String())
			}
		}
	}
}
