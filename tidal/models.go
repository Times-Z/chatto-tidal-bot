package tidal

import "io"

type SearchResult struct {
	ID       uint64
	Title    string
	Artist   string
	Duration int
}

type Track struct {
	ID       uint64
	Title    string
	Artist   string
	Duration int
}

type TrackStream struct {
	Reader   io.ReadCloser
	Duration int
}
