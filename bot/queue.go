package bot

import "sync"

// Track holds metadata for a single queued track and its requestor.
type Track struct {
	TID       uint64
	Title     string
	Artist    string
	Duration  int
	Requestor string
}

// Queue is a FIFO track queue with cursor-based iteration.
// Tracks are added at the end and consumed sequentially via Next().
// Len(), Current(), and List() are relative to the current cursor position.
type Queue struct {
	mu     sync.Mutex
	tracks []Track
	pos    int
}

// NewQueue creates an empty Queue.
func NewQueue() *Queue {
	return &Queue{}
}

// Add appends a track to the end of the queue.
func (q *Queue) Add(t Track) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tracks = append(q.tracks, t)
}

// Next returns the next track at the current cursor position and advances
// the cursor. Returns false if the queue has been fully consumed.
func (q *Queue) Next() (Track, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pos >= len(q.tracks) {
		return Track{}, false
	}
	t := q.tracks[q.pos]
	q.pos++
	return t, true
}

// Current returns the track currently at the cursor position
// (the most recently returned by Next()), without advancing.
// Returns false if no track has been consumed yet.
func (q *Queue) Current() (Track, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pos == 0 || q.pos > len(q.tracks) {
		return Track{}, false
	}
	return q.tracks[q.pos-1], true
}

// Skip advances the cursor past the current track without playing it.
// Returns false if the queue is already fully consumed.
func (q *Queue) Skip() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pos >= len(q.tracks) {
		return false
	}
	q.pos++
	return true
}

// Clear removes all tracks from the queue and resets the cursor.
func (q *Queue) Clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tracks = nil
	q.pos = 0
}

// Len returns the number of tracks remaining from the current cursor position.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tracks) - q.pos
}

// List returns a copy of all remaining tracks from the current cursor position.
func (q *Queue) List() []Track {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pos >= len(q.tracks) {
		return nil
	}
	out := make([]Track, len(q.tracks)-q.pos)
	copy(out, q.tracks[q.pos:])
	return out
}
