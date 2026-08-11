package agent

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"smokeping-modern/internal/agentwire"
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

var (
	errShortFrame = errors.New("agent/spool: short frame")
	errBadFrame   = errors.New("agent/spool: bad frame checksum")
)

const frameHeader = 8 // u32 len + u32 crc
const headFile = "head"

// encodeFrame appends one framed record to dst and returns the extended slice.
// frame = [u32 len][u32 crc32c(payload)][payload]; payload = [u64 seq][body].
func encodeFrame(dst []byte, seq int64, body []byte) []byte {
	payloadLen := 8 + len(body)
	var hdr [frameHeader]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(payloadLen))
	// build payload contiguously so the CRC covers seq+body
	payload := make([]byte, payloadLen)
	binary.LittleEndian.PutUint64(payload[0:8], uint64(seq))
	copy(payload[8:], body)
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.Checksum(payload, crcTable))
	dst = append(dst, hdr[:]...)
	dst = append(dst, payload...)
	return dst
}

// decodeFrame reads one frame from the front of src. It returns errShortFrame
// (with n==0) when src does not yet hold a complete frame — the caller treats
// that as a torn tail — and errBadFrame when the checksum does not match.
func decodeFrame(src []byte) (seq int64, body []byte, n int, err error) {
	if len(src) < frameHeader {
		return 0, nil, 0, errShortFrame
	}
	payloadLen := int(binary.LittleEndian.Uint32(src[0:4]))
	if payloadLen < 8 {
		return 0, nil, 0, errBadFrame
	}
	crc := binary.LittleEndian.Uint32(src[4:8])
	if len(src) < frameHeader+payloadLen {
		return 0, nil, 0, errShortFrame
	}
	payload := src[frameHeader : frameHeader+payloadLen]
	if crc32.Checksum(payload, crcTable) != crc {
		return 0, nil, 0, errBadFrame
	}
	seq = int64(binary.LittleEndian.Uint64(payload[0:8]))
	body = payload[8:]
	return seq, body, frameHeader + payloadLen, nil
}

type record struct {
	seq  int64
	body []byte
}

// readSegment decodes every complete, checksum-valid frame from path, in order. It returns the
// decoded records, the number of bytes it consumed (the offset of the first undecodable byte), and
// the decode's stop reason: nil if the whole file decoded cleanly (reached EOF), errShortFrame for
// a truncated final frame (a crash-torn tail), or errBadFrame for a checksum mismatch (corruption).
// It does NOT modify the file: the caller decides what an incomplete decode means — a torn tail on
// the active segment is truncated to consumed, while a checksum failure (any segment) or any
// incomplete decode of a closed segment is fatal (CODE_REVIEW M3). A missing/unreadable file is a
// hard error.
func readSegment(path string) (recs []record, consumed int64, stopErr error, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, nil, err
	}
	off := 0
	for off < len(data) {
		seq, body, n, derr := decodeFrame(data[off:])
		if derr != nil {
			stopErr = derr // errShortFrame (torn tail) or errBadFrame (corruption)
			break
		}
		cp := make([]byte, len(body))
		copy(cp, body)
		recs = append(recs, record{seq: seq, body: cp})
		off += n
	}
	return recs, int64(off), stopErr, nil
}

// fsyncDir flushes a directory entry change (a rename or create) to disk, so it
// survives a crash independently of filesystem metadata-ordering guarantees.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// writeHead atomically records the head watermark: write a temp file (with a CRC),
// fsync it, then rename over dir/head. Rename is atomic on a single filesystem, so
// a crash leaves either the old value or the new one, never a torn file. The
// directory is fsynced after the rename so the new watermark is durable independently
// of filesystem metadata-ordering guarantees, before the caller (flushLocked) may act
// on it by reclaiming (deleting) segments the new watermark marks dead.
func writeHead(dir string, head int64) error {
	var buf [12]byte // u64 head + u32 crc
	binary.LittleEndian.PutUint64(buf[0:8], uint64(head))
	binary.LittleEndian.PutUint32(buf[8:12], crc32.Checksum(buf[0:8], crcTable))
	tmp, err := os.CreateTemp(dir, "head-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(buf[:]); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, headFile)); err != nil {
		return err
	}
	return fsyncDir(dir)
}

// readHead returns the durable head watermark, or 0 when the file is absent (first
// run). A present-but-corrupt file returns an error.
func readHead(dir string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(dir, headFile))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(data) != 12 || binary.LittleEndian.Uint32(data[8:12]) != crc32.Checksum(data[0:8], crcTable) {
		return 0, errBadFrame
	}
	return int64(binary.LittleEndian.Uint64(data[0:8])), nil
}

const (
	fsyncEvery      = time.Second
	segmentMaxBytes = 64 << 20
	segGlob         = "seg-*.log"
)

func segmentName(firstSeq int64) string { return fmt.Sprintf("seg-%020d.log", firstSeq) }

// lockDir acquires an exclusive, non-blocking flock on dir/spool.lock. The returned
// file must stay open for the lock to be held; close() releases it. A contended lock
// (another agent already holds it) is a returned error.
func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "spool.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("spool dir %s is locked by another agent: %w", dir, err)
	}
	return f, nil
}

type stagedData struct {
	seq int64
	r   agentwire.RoundReport
}

type segMeta struct {
	path   string
	maxSeq int64 // max DATA seq in this closed segment (-1 if none)
}

type spool struct {
	dir    string
	segMax int64 // == segmentMaxBytes; overridable in tests

	lockFile *os.File // exclusive flock on dir/spool.lock; held for the spool's lifetime
	writeErr error    // test seam: when set, writeRecordLocked returns it (simulates a disk error)

	mu          sync.Mutex
	active      *os.File
	activePath  string
	activeBytes int64
	activeMax   int64 // max DATA seq in the active segment (-1 if none)
	segments    []segMeta

	pending     []stagedData
	head        int64 // latest staged head watermark
	headDurable int64 // latest fsynced head watermark

	degraded bool
	closed   bool

	replayedN  int
	errN       int
	reclaimedN int

	started   bool
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// openSpool replays the existing segments in dir to recover the live round set and head
// watermark, truncates a crash-torn tail on the last (active) segment, fails on corruption in a
// closed segment, deletes fully-dead closed segments, and opens the active segment for appending.
// Recovery is bounded by capRounds/maxBytes (the running buffer's budget): the oldest live rounds
// are evicted while decoding, so a full spool cannot OOM a constrained vantage on restart, and the
// returned head reflects any such eviction. It returns the (possibly advanced) head and the
// bounded live rounds (contiguous, seq order) for the buffer to adopt. The background flusher is
// NOT started here — the caller calls start().
func openSpool(dir string, capRounds, maxBytes int) (*spool, int64, []agentwire.RoundReport, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, 0, nil, fmt.Errorf("create spool dir: %w", err)
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, 0, nil, err
	}
	head, err := readHead(dir)
	if err != nil {
		lock.Close()
		return nil, 0, nil, fmt.Errorf("read head: %w", err)
	}
	segPaths, err := filepath.Glob(filepath.Join(dir, segGlob))
	if err != nil {
		lock.Close()
		return nil, 0, nil, err
	}
	sort.Strings(segPaths) // zero-padded names sort by firstSeq

	s := &spool{dir: dir, segMax: segmentMaxBytes, lockFile: lock, head: head, headDurable: head, activeMax: -1}

	// Decode every segment in seq order, retaining only per-segment metadata (path/maxSeq) plus a
	// bounded live queue — never every segment's decoded bodies at once (CODE_REVIEW #1). The live
	// queue is bounded by the same count/byte budget the running buffer enforces, evicting the
	// oldest live rounds during decode so recovering a full spool cannot OOM a constrained vantage.
	type segInfo struct {
		path   string
		maxSeq int64
	}
	var segs []segInfo
	var live []agentwire.RoundReport
	liveBytes, dropped := 0, 0
	wantSeq := int64(-1) // -1 until the first record fixes the expected next sequence
	for i, p := range segPaths {
		isLast := i == len(segPaths)-1
		recs, consumed, stopErr, rerr := readSegment(p)
		if rerr != nil {
			lock.Close()
			return nil, 0, nil, fmt.Errorf("read segment %s: %w", p, rerr)
		}
		if stopErr != nil {
			switch {
			case isLast && errors.Is(stopErr, errShortFrame):
				// The active segment being appended when the process crashed can end in a torn
				// tail (a truncated final frame); truncate to the last good frame so appends
				// resume cleanly. This is the ONLY case truncation is permitted.
				if terr := os.Truncate(p, consumed); terr != nil {
					lock.Close()
					return nil, 0, nil, fmt.Errorf("truncate torn segment %s: %w", p, terr)
				}
			case isLast:
				// A checksum mismatch (errBadFrame) in the active segment is real corruption, not
				// a crash-torn tail. Fail loudly instead of silently truncating the bad frame and
				// every valid frame after it — which would lose durable backlog with no operator
				// signal, break the buffer's contiguous-sequence assumption, and reuse/replay
				// sequence numbers (CODE_REVIEW M3).
				lock.Close()
				return nil, 0, nil, fmt.Errorf("spool: active segment %s is corrupt at byte %d (checksum mismatch, not a torn tail); clear the spool dir to recover (the buffered backlog is lost)", p, consumed)
			default:
				// A closed segment was synced and closed before its successor was created, so any
				// short/bad frame there is corruption, not a crash tail. Fail loudly instead of
				// silently returning the valid prefix: dropping its (and every later segment's)
				// records would break the buffer's contiguous-sequence assumption and reuse/replay
				// sequence numbers (CODE_REVIEW #2).
				lock.Close()
				return nil, 0, nil, fmt.Errorf("spool: closed segment %s is corrupt at byte %d; clear the spool dir to recover (the buffered backlog is lost)", p, consumed)
			}
		}
		maxSeq := int64(-1)
		for _, rec := range recs {
			if wantSeq >= 0 && rec.seq != wantSeq {
				// Records must form one strictly-increasing contiguous run; a gap/duplicate that
				// the per-frame CRC did not catch still breaks adoption, so fail (CODE_REVIEW #2).
				lock.Close()
				return nil, 0, nil, fmt.Errorf("spool: non-contiguous sequence in %s: got %d, want %d", p, rec.seq, wantSeq)
			}
			wantSeq = rec.seq + 1
			if rec.seq > maxSeq {
				maxSeq = rec.seq
			}
			if rec.seq >= head {
				var r agentwire.RoundReport
				if uerr := json.Unmarshal(rec.body, &r); uerr != nil {
					// A CRC-valid frame that will not decode is corruption; skipping it would open
					// the same silent-gap hole as a dropped frame, so fail (CODE_REVIEW #2).
					lock.Close()
					return nil, 0, nil, fmt.Errorf("spool: undecodable live record seq %d in %s: %w", rec.seq, p, uerr)
				}
				sz := estimatedJSONBytes(r)
				for len(live) > 0 && (len(live) >= capRounds || liveBytes+sz > maxBytes) {
					liveBytes -= estimatedJSONBytes(live[0])
					// Zero the evicted round so its RTTs slice and strings are collectable now,
					// rather than lingering in the shared backing array until append reallocates —
					// keeps the transient recovery footprint near the budget, not ~2x it.
					live[0] = agentwire.RoundReport{}
					live = live[1:]
					dropped++
				}
				live = append(live, r)
				liveBytes += sz
			}
		}
		segs = append(segs, segInfo{path: p, maxSeq: maxSeq})
	}
	// Rounds evicted above are dead; advance the in-memory watermark past them. headDurable stays
	// at the persisted `head` until the next flush persists effectiveHead, so reclamation below
	// (and any crash before that flush) still keys off the durable value.
	effectiveHead := head + int64(dropped)
	s.head = effectiveHead

	// Delete fully-dead closed segments per the DURABLE watermark (head); keep the rest. The last
	// segment is always kept as the active append target (even if dead — we continue appending to
	// it). Segments holding rounds evicted above (maxSeq >= head) are retained until effectiveHead
	// is persisted, then reclaimed by a later flush.
	for i, sg := range segs {
		isLast := i == len(segs)-1
		if !isLast && sg.maxSeq < head {
			os.Remove(sg.path)
			s.reclaimedN++
			continue
		}
		if !isLast {
			s.segments = append(s.segments, segMeta{path: sg.path, maxSeq: sg.maxSeq})
		}
	}

	// Open (or create) the active segment.
	var activePath string
	var activeMax int64 = -1
	created := false
	if len(segs) > 0 {
		last := segs[len(segs)-1]
		activePath = last.path
		activeMax = last.maxSeq
	} else {
		activePath = filepath.Join(dir, segmentName(head))
		created = true
	}
	f, err := os.OpenFile(activePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		lock.Close()
		return nil, 0, nil, fmt.Errorf("open active segment: %w", err)
	}
	// A brand-new first segment's directory entry needs an explicit dir fsync: per
	// fsync(2), fsyncing the file itself does not guarantee the new directory entry is
	// durable. Without this, a crash before the watermark ever advances (writeHead) or
	// the segment ever rolls (rollLocked) — the only other two dir-fsync points — could
	// lose the whole first segment. A reused last segment's dentry is already durable;
	// it survived to this startup.
	if created {
		if err := fsyncDir(dir); err != nil {
			f.Close()
			lock.Close()
			return nil, 0, nil, fmt.Errorf("fsync spool dir: %w", err)
		}
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		lock.Close() // release flock on this error path like the others
		return nil, 0, nil, fmt.Errorf("stat active segment: %w", err)
	}
	s.active = f
	s.activePath = activePath
	s.activeBytes = fi.Size()
	s.activeMax = activeMax
	s.replayedN = len(live)
	return s, effectiveHead, live, nil
}

func (s *spool) append(seq int64, r agentwire.RoundReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.degraded || s.closed {
		return
	}
	s.pending = append(s.pending, stagedData{seq: seq, r: r})
}

func (s *spool) advanceHead(headSeq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.degraded || s.closed {
		return
	}
	if headSeq > s.head {
		s.head = headSeq
	}
}

func (s *spool) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *spool) flushLocked() error {
	if s.degraded || s.closed {
		s.pending = s.pending[:0]
		return nil
	}
	for _, pr := range s.pending {
		body, err := json.Marshal(pr.r)
		if err != nil { // effectively impossible for RoundReport; skip + count, don't degrade
			s.errN++
			slog.Warn("spool: dropping unmarshalable round", "seq", pr.seq, "err", err)
			continue
		}
		if err := s.writeRecordLocked(pr.seq, body); err != nil {
			return s.degradeLocked(err)
		}
	}
	s.pending = s.pending[:0]
	if err := s.active.Sync(); err != nil {
		return s.degradeLocked(err)
	}
	if s.head > s.headDurable {
		if err := writeHead(s.dir, s.head); err != nil {
			return s.degradeLocked(err)
		}
		s.headDurable = s.head
		s.reclaimLocked()
	}
	return nil
}

// writeRecordLocked appends one framed DATA record to the active segment, rolling a new
// segment first when the record would exceed segMax.
func (s *spool) writeRecordLocked(seq int64, body []byte) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	frame := encodeFrame(nil, seq, body)
	if s.activeBytes > 0 && s.activeBytes+int64(len(frame)) > s.segMax {
		if err := s.rollLocked(seq); err != nil {
			return err
		}
	}
	if _, err := s.active.Write(frame); err != nil {
		return err
	}
	s.activeBytes += int64(len(frame))
	if seq > s.activeMax {
		s.activeMax = seq
	}
	return nil
}

func (s *spool) rollLocked(nextSeq int64) error {
	if err := s.active.Sync(); err != nil {
		return err
	}
	if err := s.active.Close(); err != nil {
		return err
	}
	s.segments = append(s.segments, segMeta{path: s.activePath, maxSeq: s.activeMax})
	p := filepath.Join(s.dir, segmentName(nextSeq))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	// Fsync the directory so the new segment's directory entry is durable before any
	// records are written+fsynced into it — otherwise a crash could lose an entire
	// freshly-rolled segment's fsynced rounds if the rename/create never made it to disk.
	if err := fsyncDir(s.dir); err != nil {
		f.Close()
		return err
	}
	s.active = f
	s.activePath = p
	s.activeBytes = 0
	s.activeMax = -1
	return nil
}

// reclaimLocked deletes closed segments whose every record is now dead (maxSeq < headDurable).
func (s *spool) reclaimLocked() {
	kept := s.segments[:0]
	for _, m := range s.segments {
		if m.maxSeq < s.headDurable {
			os.Remove(m.path)
			s.reclaimedN++
			continue
		}
		kept = append(kept, m)
	}
	s.segments = kept
}

func (s *spool) degradeLocked(err error) error {
	s.degraded = true
	s.errN++
	s.pending = nil
	slog.Error("spool write failed, degrading to memory-only", "err", err, "dir", s.dir)
	return err
}

func (s *spool) start() {
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.mu.Unlock()
	go s.flushLoop()
}

func (s *spool) flushLoop() {
	defer close(s.done)
	t := time.NewTicker(fsyncEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if err := s.flush(); err != nil {
				// already logged in degradeLocked; nothing more to do
			}
		}
	}
}

// close stops the background flusher (if started), performs a final flush, and releases the
// active file. It is safe to call any number of times, including concurrently: closeOnce
// guarantees the stop/flush/release sequence below runs exactly once; every call after the
// first is a no-op that returns nil without re-closing s.stop, re-flushing, or touching the
// (already-nil) active file.
func (s *spool) close() error {
	var ferr error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		started := s.started
		s.mu.Unlock()
		if started {
			close(s.stop)
			<-s.done
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.degraded {
			ferr = s.flushLocked()
		}
		s.closed = true
		if s.active != nil {
			s.active.Close()
			s.active = nil
		}
		if s.lockFile != nil {
			syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN)
			s.lockFile.Close()
			s.lockFile = nil
		}
	})
	return ferr
}

func (s *spool) replayed() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.replayedN }
func (s *spool) errors() int    { s.mu.Lock(); defer s.mu.Unlock(); return s.errN }
func (s *spool) reclaimed() int { s.mu.Lock(); defer s.mu.Unlock(); return s.reclaimedN }
