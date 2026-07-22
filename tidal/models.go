// Package tidal provides data models for Tidal API responses, search results,
// and audio stream metadata.
package tidal

import (
	"fmt"
	"io"
	"strings"

	"github.com/binozo/go-tiddl"
)

// Quality constants that can be used in config.json for tidal_quality.
const (
	QualityLow           = "LOW"
	QualityHigh          = "HIGH"
	QualityLossless      = "LOSSLESS"
	QualityHiResLossless = "HI_RES_LOSSLESS"
)

// ParseQuality converts a config string to a go-tiddl AudioQuality.
// Returns empty (meaning "best available") for unrecognized values.
func ParseQuality(s string) tiddl.AudioQuality {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case QualityLow:
		return tiddl.Low
	case QualityHigh:
		return tiddl.High
	case QualityLossless:
		return tiddl.Lossless
	case QualityHiResLossless:
		return tiddl.HiResLossless
	default:
		return ""
	}
}

// SearchResult represents a track returned from a Tidal search query,
// containing enough metadata to display and queue the track.
type SearchResult struct {
	ID       uint64
	Title    string
	Artist   string
	Duration int
}

// Track represents a resolved Tidal track with full metadata.
// This is returned when fetching a specific track by ID rather than searching.
type Track struct {
	ID       uint64
	Title    string
	Artist   string
	Duration int
}

// TrackStream wraps an audio stream reader with metadata about the track
// and its audio quality (codec, bit depth, sample rate).
type TrackStream struct {
	Reader     io.ReadCloser
	Duration   int
	Quality    string
	Codec      string
	BitDepth   int
	SampleRate int
}

// FormatAudioInfo returns a human-readable string describing the stream's
// audio quality (e.g. "FLAC 24bit 48kHz" or "AAC 16bit 44.1kHz").
func (s *TrackStream) FormatAudioInfo() string {
	codec := s.Codec
	switch codec {
	case "flac":
		codec = "FLAC"
	case "mp4a.40.5":
		codec = "HE-AAC"
	case "mp4a.40.2":
		codec = "AAC-LC"
	case "mp4a.40.34":
		codec = "AAC-LD"
	case "mha1":
		codec = "MPEG-H"
	default:
		if codec != "" {
			codec = s.Quality
		}
	}
	if s.BitDepth > 0 && s.SampleRate > 0 {
		sr := s.SampleRate
		if sr >= 1000 {
			return fmt.Sprintf("%s %dbit %dkHz", codec, s.BitDepth, sr/1000)
		}
		return fmt.Sprintf("%s %dbit %dHz", codec, s.BitDepth, sr)
	}
	if codec != "" {
		return codec
	}
	return s.Quality
}
