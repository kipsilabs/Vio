package playback

import "testing"

func TestParseWxHResolution(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantW  int
		wantH  int
		wantOK bool
	}{
		{name: "1080p WxH", input: "1920x1080", wantW: 1920, wantH: 1080, wantOK: true},
		{name: "4K WxH", input: "3840x2160", wantW: 3840, wantH: 2160, wantOK: true},
		{name: "720p WxH", input: "1280x720", wantW: 1280, wantH: 720, wantOK: true},
		{name: "uppercase X", input: "1920X1080", wantW: 1920, wantH: 1080, wantOK: true},
		{name: "surrounding whitespace", input: "  1920x1080  ", wantW: 1920, wantH: 1080, wantOK: true},
		{name: "trailing junk rejected", input: "1920x1080garbage"},
		{name: "decimal rejected", input: "1920x1080.5"},
		{name: "zero rejected", input: "0x0"},
		{name: "negative rejected", input: "-1x-1"},
		{name: "overflow rejected", input: "99999x99999"},
		{name: "empty", input: ""},
		{name: "standard label rejected", input: "1080p"},
		{name: "single number rejected", input: "1920"},
		{name: "missing width", input: "x1080"},
		{name: "missing height", input: "1920x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, h, ok := parseWxHResolution(tt.input)
			if w != tt.wantW || h != tt.wantH || ok != tt.wantOK {
				t.Fatalf("parseWxHResolution(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tt.input, w, h, ok, tt.wantW, tt.wantH, tt.wantOK)
			}
		})
	}

	dimensions := []struct {
		name  string
		input string
		wantW int
		wantH int
	}{
		{name: "WxH input", input: "1920x1080", wantW: 1920, wantH: 1080},
		{name: "label still works", input: "1080p", wantW: 1920, wantH: 1080},
	}
	for _, tt := range dimensions {
		t.Run("dimensions/"+tt.name, func(t *testing.T) {
			w, h := dimensionsFromResolutionV3(tt.input)
			if w != tt.wantW || h != tt.wantH {
				t.Fatalf("dimensionsFromResolutionV3(%q) = (%d, %d), want (%d, %d)",
					tt.input, w, h, tt.wantW, tt.wantH)
			}
		})
	}
}
