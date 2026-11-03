package transcodenode

import "errors"

var ErrCopyVideoMPEGTSAttestationMismatch = errors.New("transcode node copy-video MPEG-TS attestation mismatch")

func ValidateCopyVideoMPEGTSAttestation(req TranscodeStartRequest, response TranscodeStartResponse) error {
	if req.CopyVideoMPEGTS && !response.CopyVideoMPEGTS {
		return ErrCopyVideoMPEGTSAttestationMismatch
	}
	return nil
}
