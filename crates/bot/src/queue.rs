#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Track {
    pub tid: u64,
    pub title: String,
    pub artist: String,
    pub duration: i32,
    pub requestor: String,
    pub cover_url: String,
}

#[derive(Debug, Default)]
pub struct Queue {
    tracks: Vec<Track>,
    pos: usize,
}

impl Queue {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn add(&mut self, track: Track) {
        self.tracks.push(track);
    }

    pub fn dequeue(&mut self) -> Option<Track> {
        if self.pos >= self.tracks.len() {
            return None;
        }
        let track = self.tracks[self.pos].clone();
        self.pos += 1;
        Some(track)
    }

    pub fn current(&self) -> Option<&Track> {
        if self.pos == 0 || self.pos > self.tracks.len() {
            return None;
        }
        self.tracks.get(self.pos - 1)
    }

    pub fn skip(&mut self) -> bool {
        if self.pos >= self.tracks.len() {
            return false;
        }
        self.pos += 1;
        true
    }

    pub fn total_len(&self) -> usize {
        self.tracks.len()
    }

    pub fn clear(&mut self) {
        self.tracks.clear();
        self.pos = 0;
    }

    pub fn len(&self) -> usize {
        self.tracks.len().saturating_sub(self.pos)
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    pub fn list(&self) -> &[Track] {
        if self.pos >= self.tracks.len() {
            return &[];
        }
        &self.tracks[self.pos..]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn track(tid: u64, title: &str, duration: i32) -> Track {
        Track {
            tid,
            title: title.to_owned(),
            artist: String::new(),
            duration,
            requestor: String::new(),
            cover_url: String::new(),
        }
    }

    #[test]
    fn queue_add_and_len() {
        let mut q = Queue::new();
        assert_eq!(q.len(), 0);

        q.add(track(1, "A", 100));
        q.add(track(2, "B", 200));
        assert_eq!(q.len(), 2);
    }

    #[test]
    fn queue_next() {
        let mut q = Queue::new();
        q.add(track(1, "A", 100));
        q.add(track(2, "B", 200));

        let t1 = q.dequeue().unwrap();
        assert_eq!(t1.tid, 1);
        assert_eq!(q.len(), 1);

        let t2 = q.dequeue().unwrap();
        assert_eq!(t2.tid, 2);
        assert_eq!(q.len(), 0);

        assert!(q.dequeue().is_none());
    }

    #[test]
    fn queue_current() {
        let mut q = Queue::new();
        assert!(q.current().is_none());

        q.add(track(1, "A", 100));
        assert!(q.current().is_none());

        q.dequeue();
        assert_eq!(q.current().unwrap().tid, 1);
    }

    #[test]
    fn queue_skip() {
        let mut q = Queue::new();
        q.add(track(1, "A", 100));
        q.add(track(2, "B", 200));

        assert!(q.skip());
        assert_eq!(q.len(), 1);
        assert_eq!(q.dequeue().unwrap().tid, 2);
        assert!(!q.skip());
    }

    #[test]
    fn queue_clear() {
        let mut q = Queue::new();
        q.add(track(1, "A", 100));
        q.add(track(2, "B", 200));
        q.clear();

        assert_eq!(q.len(), 0);
        assert!(q.dequeue().is_none());
    }

    #[test]
    fn queue_list() {
        let mut q = Queue::new();
        q.add(track(1, "A", 100));
        q.add(track(2, "B", 200));

        let list = q.list();
        assert_eq!(list.len(), 2);

        q.dequeue();
        let list = q.list();
        assert_eq!(list.len(), 1);
        assert_eq!(list[0].tid, 2);
    }
}
