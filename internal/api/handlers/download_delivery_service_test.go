package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/downloads"
)

// A managed file transfer that fails after the response started must abort the
// connection. Returning nil would let a chunked or known-length response end as
// a complete file, so a browser would accept a truncated download. The warning
// is for server faults only: a client that left has canceled the request.
func TestServeDownloadFileAbortsAfterResponseCommitted(t *testing.T) {
	for _, clientLeft := range []bool{false, true} {
		t.Run(fmt.Sprintf("client_left=%v", clientLeft), func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			svc := &fakeDownloadService{
				serveBody: "partial",
				serveErr:  fmt.Errorf("virtual download transfer interrupted: %w", downloads.ErrResponseCommitted),
			}
			req := downloadTestRequest(http.MethodGet, "/downloads/dl1/file", nil, 7, "pA", "")
			if clientLeft {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			rec := httptest.NewRecorder()
			func() {
				defer func() {
					if got := recover(); got != http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
						t.Fatalf("panic = %v, want http.ErrAbortHandler", got)
					}
				}()
				if err := NewDownloadHandler(svc).ServeDownloadFile(rec, req, "dl1", false); err != nil {
					t.Fatalf("ServeDownloadFile error = %v, want an abort instead", err)
				}
			}()

			if rec.Body.String() != "partial" {
				t.Fatalf("body = %q, want the partial file only", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "internal_error") {
				t.Fatal("an error body was appended after the partial file")
			}
			logged := strings.Contains(logs.String(), "download failed after the response started")
			if logged == clientLeft || strings.Contains(logs.String(), "level=ERROR") {
				t.Fatalf("client left = %v, logs = %q", clientLeft, logs.String())
			}
		})
	}
}

// A non-committed failure still travels back to the caller so each transport can
// write its own error document.
func TestServeDownloadFileReturnsFailuresBeforeTheResponseStarts(t *testing.T) {
	svc := &fakeDownloadService{serveErr: downloads.ErrNotFound}
	err := NewDownloadHandler(svc).ServeDownloadFile(httptest.NewRecorder(), downloadTestRequest(http.MethodGet, "/downloads/dl1/file", nil, 7, "pA", ""), "dl1", false)
	if !errors.Is(err, downloads.ErrNotFound) {
		t.Fatalf("ServeDownloadFile error = %v, want ErrNotFound", err)
	}
}

// stalledDownloadService writes part of a file, flushes it so the response is
// committed, then reports the transfer as broken.
type stalledDownloadService struct {
	*fakeDownloadService
	body       string
	declared   bool
	flushError error
}

func (s *stalledDownloadService) ServeFile(_ context.Context, w http.ResponseWriter, _ *http.Request, _ int, _, _, _ string, _ catalog.AccessFilter) error {
	if s.declared {
		// Promise more than this transfer will deliver, so the abort is a
		// genuinely truncated body rather than a complete one.
		w.Header().Set("Content-Length", strconv.Itoa(len(s.body)+64))
	}
	if _, err := w.Write([]byte(s.body)); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if s.flushError != nil {
		return s.flushError
	}
	return fmt.Errorf("serving download: %w", downloads.ErrResponseCommitted)
}

// The abort has to be observable on the wire: a client reading an aborted
// known-length or chunked response must get a read error, never a clean body.
func TestServeDownloadFileAbortIsVisibleToClient(t *testing.T) {
	const body = "partial-bytes"
	for _, testCase := range []struct {
		name     string
		declared bool
	}{
		{name: "declared length", declared: true},
		{name: "unknown length"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			svc := &stalledDownloadService{fakeDownloadService: &fakeDownloadService{}, body: body, declared: testCase.declared}
			handler := NewDownloadHandler(svc)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The client's context never crosses the wire; install the
				// authenticated identity the API middleware would set.
				ctx := apimw.SetClaims(r.Context(), &auth.Claims{
					UserID:    7,
					Role:      "user",
					TokenType: auth.TokenTypeAccess,
				})
				ctx = apimw.SetProfileID(ctx, "pA")
				if err := handler.ServeDownloadFile(w, r.WithContext(ctx), "dl1", false); err != nil {
					t.Errorf("ServeDownloadFile error = %v, want an abort instead", err)
				}
			}))
			defer server.Close()

			req, err := http.NewRequest(http.MethodGet, server.URL+"/downloads/dl1/file", nil)
			if err != nil {
				t.Fatalf("request = %v", err)
			}
			response, err := server.Client().Do(req)
			if err != nil {
				// A reset before the headers is still an abort, but the test
				// wants to see the committed response carry the broken transfer.
				t.Fatalf("request error = %v, want a committed response that then aborts", err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.StatusCode)
			}
			received, readErr := io.ReadAll(response.Body)
			if readErr == nil {
				t.Fatalf("client read %d bytes without an error, want an aborted transfer", len(received))
			}
			if len(received) == 0 {
				t.Fatal("client received no bytes; the abort must follow a started response")
			}
		})
	}
}
