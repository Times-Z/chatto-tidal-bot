package tidal

import (
	"testing"
)

func TestFormatAudioInfo(t *testing.T) {
	tests := []struct {
		name   string
		stream TrackStream
		want   string
	}{
		{
			name:   "FLAC hi-res",
			stream: TrackStream{Quality: "HI_RES_LOSSLESS", Codec: "flac", BitDepth: 24, SampleRate: 48000},
			want:   "FLAC 24bit 48kHz",
		},
		{
			name:   "FLAC CD",
			stream: TrackStream{Quality: "LOSSLESS", Codec: "flac", BitDepth: 16, SampleRate: 44100},
			want:   "FLAC 16bit 44kHz",
		},
		{
			name:   "AAC-LC",
			stream: TrackStream{Quality: "HIGH", Codec: "mp4a.40.2", BitDepth: 16, SampleRate: 44100},
			want:   "AAC-LC 16bit 44kHz",
		},
		{
			name:   "HE-AAC",
			stream: TrackStream{Quality: "LOW", Codec: "mp4a.40.5", BitDepth: 16, SampleRate: 44100},
			want:   "HE-AAC 16bit 44kHz",
		},
		{
			name:   "AAC-LD",
			stream: TrackStream{Quality: "HIGH", Codec: "mp4a.40.34", BitDepth: 16, SampleRate: 48000},
			want:   "AAC-LD 16bit 48kHz",
		},
		{
			name:   "MPEG-H",
			stream: TrackStream{Quality: "HI_RES_LOSSLESS", Codec: "mha1", BitDepth: 24, SampleRate: 96000},
			want:   "MPEG-H 24bit 96kHz",
		},
		{
			name:   "unknown codec falls back to quality",
			stream: TrackStream{Quality: "LOSSLESS", Codec: "unknown", BitDepth: 0, SampleRate: 0},
			want:   "LOSSLESS",
		},
		{
			name:   "no codec falls back to quality",
			stream: TrackStream{Quality: "HIGH", Codec: "", BitDepth: 0, SampleRate: 0},
			want:   "HIGH",
		},
		{
			name:   "sample rate below 1000 shows Hz",
			stream: TrackStream{Quality: "LOW", Codec: "flac", BitDepth: 16, SampleRate: 800},
			want:   "FLAC 16bit 800Hz",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.stream.FormatAudioInfo()
			if got != tc.want {
				t.Fatalf("FormatAudioInfo() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseQuality(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"LOW", "LOW"},
		{"low", "LOW"},
		{"HIGH", "HIGH"},
		{"high", "HIGH"},
		{"LOSSLESS", "LOSSLESS"},
		{"Lossless", "LOSSLESS"},
		{"HI_RES_LOSSLESS", "HI_RES_LOSSLESS"},
		{"hi_res_lossless", "HI_RES_LOSSLESS"},
		{"", ""},
		{"INVALID", ""},
		{"  high  ", "HIGH"},
	}

	for _, tc := range tests {
		got := string(ParseQuality(tc.input))
		if got != tc.want {
			t.Fatalf("ParseQuality(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseQualityRoundTrip(t *testing.T) {
	for _, q := range []string{"LOW", "HIGH", "LOSSLESS", "HI_RES_LOSSLESS"} {
		parsed := ParseQuality(q)
		if string(parsed) != q {
			t.Fatalf("ParseQuality(%q) = %q, want %q", q, string(parsed), q)
		}
	}
}

func TestArtistName(t *testing.T) {
	makeArtists := func(names ...string) []struct {
		Name string `json:"name"`
	} {
		out := make([]struct {
			Name string `json:"name"`
		}, len(names))
		for i, n := range names {
			out[i].Name = n
		}
		return out
	}

	tests := []struct {
		name  string
		track tidalTrack
		want  string
	}{
		{
			name:  "uses ArtistName when set",
			track: tidalTrack{ArtistName: "Daft Punk", Artists: makeArtists("Other")},
			want:  "Daft Punk",
		},
		{
			name:  "falls back to first artist",
			track: tidalTrack{Artists: makeArtists("Daft Punk")},
			want:  "Daft Punk",
		},
		{
			name:  "empty when neither set",
			track: tidalTrack{},
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := artistName(tc.track)
			if got != tc.want {
				t.Fatalf("artistName() = %q, want %q", got, tc.want)
			}
		})
	}
}
