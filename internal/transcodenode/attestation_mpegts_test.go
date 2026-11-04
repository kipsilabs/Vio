package transcodenode

import "testing"

func TestValidateCopyVideoMPEGTSAttestation(t *testing.T) {
	tests := []struct {
		name     string
		request  bool
		response bool
		wantErr  bool
	}{
		{name: "copy requested and confirmed", request: true, response: true},
		{name: "copy not requested", request: false, response: false},
		{name: "copy requested but missing", request: true, wantErr: true},
		{name: "copy requested but declined", request: true, response: false, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCopyVideoMPEGTSAttestation(
				TranscodeStartRequest{CopyVideoMPEGTS: tt.request},
				TranscodeStartResponse{CopyVideoMPEGTS: tt.response},
			)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateCopyVideoMPEGTSAttestation() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
