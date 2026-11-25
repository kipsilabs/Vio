package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// A client that disconnects during a virtual direct-play proxy must be recorded
// as an abandoned request, not a 502 dependency failure. The 4xx status keeps
// the v2 adapter from writing a server-error problem and the discarded-body
// ERROR; genuine upstream failures keep their retryable 502.
func TestVirtualProxyFailureStatusClassifiesClientCancel(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	status, code, _ := virtualProxyFailureStatus(canceled, context.Canceled)
	if status != http.StatusRequestTimeout || code != "client_canceled" {
		t.Fatalf("client cancel classified as status=%d code=%q, want 408 client_canceled", status, code)
	}
	if status >= http.StatusInternalServerError {
		t.Fatal("a client cancellation must not be classified as a server error")
	}

	status, code, _ = virtualProxyFailureStatus(context.Background(), errors.New("upstream refused"))
	if status != http.StatusBadGateway || code != "virtual_stream_unavailable" {
		t.Fatalf("upstream failure classified as status=%d code=%q, want 502 virtual_stream_unavailable", status, code)
	}
}
