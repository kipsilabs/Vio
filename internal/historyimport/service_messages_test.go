package historyimport

import (
	"errors"
	"net/http"
	"testing"
)

func TestUserFacingRunError(t *testing.T) {
	t.Parallel()

	t.Run("unauthorized upstream errors mention server settings", func(t *testing.T) {
		t.Parallel()

		message := userFacingRunError(ExecutionSummary{}, &jellyfinHTTPError{StatusCode: http.StatusUnauthorized})
		if message != "Couldn't connect to that server. Check the URL, username, and password and try again." {
			t.Fatalf("unexpected message: %q", message)
		}
	})

	t.Run("partial runs mention best effort behavior", func(t *testing.T) {
		t.Parallel()

		summary := ExecutionSummary{Fetched: 10, Matched: 8, ProgressUpdated: 2}
		message := userFacingRunError(summary, errors.New("timeout"))
		if message != "Import stopped early. Some history may already be imported." {
			t.Fatalf("unexpected message: %q", message)
		}
	})

	t.Run("empty failures stay concise", func(t *testing.T) {
		t.Parallel()

		message := userFacingRunError(ExecutionSummary{}, errors.New("timeout"))
		if message != "Import couldn't be completed. Please try again." {
			t.Fatalf("unexpected message: %q", message)
		}
	})
}
