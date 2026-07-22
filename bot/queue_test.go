package bot

import (
	"testing"
)

func TestQueueAddAndLen(t *testing.T) {
	q := NewQueue()
	if q.Len() != 0 {
		t.Fatalf("expected empty queue, got %d", q.Len())
	}

	q.Add(Track{TID: 1, Title: "A", Duration: 100})
	q.Add(Track{TID: 2, Title: "B", Duration: 200})
	if q.Len() != 2 {
		t.Fatalf("expected len 2, got %d", q.Len())
	}
}

func TestQueueNext(t *testing.T) {
	q := NewQueue()
	q.Add(Track{TID: 1, Title: "A", Duration: 100})
	q.Add(Track{TID: 2, Title: "B", Duration: 200})

	t1, ok := q.Next()
	if !ok {
		t.Fatal("expected first track")
	}
	if t1.TID != 1 {
		t.Fatalf("expected TID 1, got %d", t1.TID)
	}
	if q.Len() != 1 {
		t.Fatalf("expected len 1 after one next, got %d", q.Len())
	}

	t2, ok := q.Next()
	if !ok {
		t.Fatal("expected second track")
	}
	if t2.TID != 2 {
		t.Fatalf("expected TID 2, got %d", t2.TID)
	}
	if q.Len() != 0 {
		t.Fatalf("expected len 0 after consuming all, got %d", q.Len())
	}

	_, ok = q.Next()
	if ok {
		t.Fatal("expected false on empty queue")
	}
}

func TestQueueCurrent(t *testing.T) {
	q := NewQueue()

	_, ok := q.Current()
	if ok {
		t.Fatal("expected false on empty queue")
	}

	q.Add(Track{TID: 1, Title: "A"})
	_, ok = q.Current()
	if ok {
		t.Fatal("expected false before any Next call")
	}

	q.Next()
	cur, ok := q.Current()
	if !ok {
		t.Fatal("expected current after next")
	}
	if cur.TID != 1 {
		t.Fatalf("expected TID 1, got %d", cur.TID)
	}
}

func TestQueueSkip(t *testing.T) {
	q := NewQueue()
	q.Add(Track{TID: 1})
	q.Add(Track{TID: 2})

	if !q.Skip() {
		t.Fatal("expected skip to succeed")
	}
	if q.Len() != 1 {
		t.Fatalf("expected len 1 after skip, got %d", q.Len())
	}

	cur, _ := q.Next()
	if cur.TID != 2 {
		t.Fatalf("expected TID 2 after skip, got %d", cur.TID)
	}

	if q.Skip() {
		t.Fatal("expected skip to fail on empty")
	}
}

func TestQueueClear(t *testing.T) {
	q := NewQueue()
	q.Add(Track{TID: 1})
	q.Add(Track{TID: 2})
	q.Clear()

	if q.Len() != 0 {
		t.Fatalf("expected len 0 after clear, got %d", q.Len())
	}

	_, ok := q.Next()
	if ok {
		t.Fatal("expected false after clear")
	}
}

func TestQueueList(t *testing.T) {
	q := NewQueue()
	q.Add(Track{TID: 1, Title: "A"})
	q.Add(Track{TID: 2, Title: "B"})

	list := q.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 items, got %d", len(list))
	}

	q.Next()
	list = q.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(list))
	}
	if list[0].TID != 2 {
		t.Fatalf("expected TID 2, got %d", list[0].TID)
	}
}

func TestQueueConcurrentSafe(t *testing.T) {
	q := NewQueue()
	done := make(chan bool)

	go func() {
		q.Add(Track{TID: 1})
		q.Add(Track{TID: 2})
		q.Add(Track{TID: 3})
		done <- true
	}()

	go func() {
		q.Len()
		q.List()
		q.Next()
		done <- true
	}()

	<-done
	<-done
}
