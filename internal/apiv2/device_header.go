package apiv2

import (
	"context"
	"net/http"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/auth"
)

// locationDeviceHeader names the device header in a validation problem.
const locationDeviceHeader = "header.x-vio-device-id"

// deviceIDShape is the identifier a client may present in X-Vio-Device-Id
// (legacy X-Silo-Device-Id accepted on ingest):
// a UUID or an opaque token of letters, digits, dot, underscore, colon and
// hyphen. Commas and interior whitespace are excluded so a header a client
// sent twice, which Huma binds as the comma-joined list value, can never be
// stored as a device identity. Surrounding whitespace is ignored, as v1's
// clamp ignores it; the 128-character bound stays with each operation's
// declared maxLength so its documented out_of_range answer is unchanged.
var deviceIDShape = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// rejectMalformedDeviceHeader refuses a request whose X-Vio-Device-Id is
// repeated or does not have the device identifier shape, before any
// operation binds it. Both spellings are checked: Vio emits X-Vio-Device-Id,
// the legacy X-Silo-Device-Id fallback is accepted on ingest. v1 reads the
// first line with Header.Get and never saw a joined value; v2 binds the
// header through Huma, which joins repeated lines with a comma, so an
// Android client that attached the header twice registered "id,id" as its
// device. A present but empty header is left to each operation's own
// required/minLength rule.
func rejectMalformedDeviceHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Merge both spellings so the checks below see a single value list;
		// when only the legacy spelling is present it is promoted to the
		// canonical header before Huma binds it.
		canonical := textproto.CanonicalMIMEHeaderKey(deviceIDHeader)
		legacy := textproto.CanonicalMIMEHeaderKey(legacyDeviceIDHeader)
		values := r.Header[canonical]
		if len(values) == 0 && len(r.Header[legacy]) > 0 {
			r.Header[canonical] = r.Header[legacy]
			values = r.Header[canonical]
		}
		switch {
		case len(values) > 1:
			writeProblem(w, r, deviceHeaderProblem("X-Vio-Device-Id must be sent once; the request carried it "+strconv.Itoa(len(values))+" times"))
			return
		case len(values) == 1:
			v := strings.TrimSpace(values[0])
			if v != "" && !deviceIDShape.MatchString(v) {
				writeProblem(w, r, deviceHeaderProblem("X-Vio-Device-Id must be a single device identifier: letters, digits, '.', '_', ':' or '-', with no comma or interior whitespace"))
				return
			}
			// Every operation then binds the same trimmed identity, as v1's
			// header clamp already trims before storing.
			r.Header.Set(deviceIDHeader, v)
		}
		// The client family header is bound by enum at each operation, so
		// promotion here only fills the canonical spelling from the legacy
		// one when the canonical header is absent. Canonical wins when both
		// are present; an invalid legacy value is left for the operation's
		// own validation to refuse.
		familyCanonical := textproto.CanonicalMIMEHeaderKey(clientFamilyHeader)
		familyLegacy := textproto.CanonicalMIMEHeaderKey(legacyClientFamilyHeader)
		if len(r.Header[familyCanonical]) == 0 && len(r.Header[familyLegacy]) > 0 {
			r.Header[familyCanonical] = r.Header[familyLegacy]
		}
		next.ServeHTTP(w, r)
	})
}

func deviceHeaderProblem(detail string) *Problem {
	return NewProblem(TypeValidationFailed, "The request did not pass validation; see errors.").
		WithErrors(ProblemError{Location: locationDeviceHeader, Code: codeInvalid, Detail: detail})
}

// withClientDevice attaches the request's X-Silo-Device-* headers for the
// login session a sign-in operation opens (auth.WithClientDevice). The id has
// already passed rejectMalformedDeviceHeader.
func withClientDevice(ctx context.Context) context.Context {
	if r := requestFrom(ctx); r != nil {
		return auth.WithClientDevice(ctx, r.Header)
	}
	return ctx
}
