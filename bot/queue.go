package bot

import "sync"

type Track struct {
	TID       uint64
	Title     string
	Artist    string
	Duration  int
	Requestor string
}

type Queue struct {
	mu     sync.Mutex
	tracks []Track
	pos    int
}

func NewQueue() *Queue {
	return &Queue{}
}

func (q *Queue) Add(t Track) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tracks = append(q.tracks, t)
}

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

func (q *Queue) Current() (Track, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pos == 0 || q.pos > len(q.tracks) {
		return Track{}, false
	}
	return q.tracks[q.pos-1], true
}

func (q *Queue) Skip() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pos >= len(q.tracks) {
		return false
	}
	q.pos++
	return true
}

func (q *Queue) Clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tracks = nil
	q.pos = 0
}

func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tracks) - q.pos
}

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
