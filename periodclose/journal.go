package periodclose

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// frameType tags journal records.
type frameType byte

const (
	frameEntry       frameType = 1
	frameCloseStart  frameType = 2
	frameCloseCommit frameType = 3
	frameReopen      frameType = 4
	frameAdjust      frameType = 5
	frameRule        frameType = 6
	frameAudit       frameType = 7
	frameCheckpoint  frameType = 8
	frameCloseAbort  frameType = 9
)

const (
	journalName = "journal.wal"
	frameMagic  = uint16(0x5A4C)
	headerSize  = 2 + 1 + 8 + 4 // magic, type, lsn, payloadLen
	crcSize     = 4
)

// ---- JSON payloads carried by opaque frames ----

type entryPayload struct {
	Entry StoredEntry
}

type closeStartPayload struct {
	Period      Period
	RuleVersion int64
	Actor       string
	At          time.Time
}

type closeCommitPayload struct {
	Version CloseVersion
	// Audit is the commit audit record; the paired start audit for a
	// successful close is embedded on the start frame instead.
}

type reopenPayload struct {
	Period      Period
	FromVersion int64
	Actor       string
	Reason      string
	At          time.Time
}

type adjustPayload struct {
	Adjustment Adjustment
}

type rulePayload struct {
	Rule Rule
}

type auditPayload struct {
	Record AuditRecord
}

type checkpointPayload struct {
	LastLSN int64
	At      time.Time
}

// closeAbortPayload rolls an interrupted "closing" period back to open.
type closeAbortPayload struct {
	Period Period
	Actor  string
	Reason string
	At     time.Time
}

// journalFrame is one append unit. Payload is already JSON-encoded.
type journalFrame struct {
	LSN     int64
	Type    frameType
	Payload []byte
}

// Journal is the local append-only, fsynced write-ahead log. A batch is
// assembled into one buffer, written in a single write(2) and fsynced, so
// recovery observes either the whole batch or none of it.
type Journal struct {
	mu      sync.Mutex
	f       *os.File
	path    string
	nextLSN int64
}

func openJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, journalName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	j := &Journal{f: f, path: path, nextLSN: 1}

	// Validate existing contents; find the end of the last intact frame.
	validEnd, lastLSN, err := j.scan(nil)
	if err != nil {
		f.Close()
		return nil, err
	}
	// Drop any torn trailing bytes so future appends stay contiguous.
	if err := f.Truncate(validEnd); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(validEnd, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	j.nextLSN = lastLSN + 1
	return j, nil
}

// scan walks the log from offset zero. When visit is nil it only locates
// the last intact frame (used at open); otherwise it invokes visit per
// frame. Returns the offset just past the last intact frame and its LSN.
func (j *Journal) scan(visit func(journalFrame) error) (int64, int64, error) {
	if _, err := j.f.Seek(0, io.SeekStart); err != nil {
		return 0, 0, err
	}
	var hdr [headerSize]byte
	var off int64
	var lastLSN int64
	for {
		_, err := io.ReadFull(j.f, hdr[:])
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break // clean end or torn header: stop at last intact frame
		}
		if err != nil {
			return 0, 0, err
		}
		magic := binary.BigEndian.Uint16(hdr[0:2])
		if magic != frameMagic {
			return 0, 0, fmt.Errorf("journal: bad magic at offset %d", off)
		}
		typ := frameType(hdr[2])
		lsn := int64(binary.BigEndian.Uint64(hdr[3:11]))
		plen := int64(binary.BigEndian.Uint32(hdr[11:15]))
		if plen < 0 || plen > 64*1024*1024 {
			return 0, 0, fmt.Errorf("journal: implausible frame length at offset %d", off)
		}
		body := make([]byte, plen+crcSize)
		if _, err := io.ReadFull(j.f, body); err != nil {
			break // torn payload/crc tail: stop at last intact frame
		}
		payload := body[:plen]
		wantCRC := binary.BigEndian.Uint32(body[plen : plen+crcSize])
		crc := crc32.NewIEEE()
		crc.Write(hdr[2:15])
		crc.Write(payload)
		if crc.Sum32() != wantCRC {
			break // corrupt frame: keep prefix intact
		}
		if visit != nil {
			if err := visit(journalFrame{LSN: lsn, Type: typ, Payload: payload}); err != nil {
				return 0, 0, err
			}
		}
		lastLSN = lsn
		off += int64(headerSize) + plen + crcSize
	}
	return off, lastLSN, nil
}

// replay invokes visit for every intact frame in LSN order.
func (j *Journal) replay(visit func(journalFrame) error) error {
	if _, err := j.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, _, err := j.scan(visit)
	return err
}

// peekNext returns the LSN that will be assigned to the next frame. All
// callers append through the Service mutex, so between peek and the append
// nothing else can advance it; appendPrepared verifies that invariant.
func (j *Journal) peekNext() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.nextLSN
}

// appendPrepared writes frames whose LSNs are already filled in by the
// caller. They must be consecutive and start exactly at peekNext().
func (j *Journal) appendPrepared(frames []journalFrame) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(frames) == 0 {
		return j.nextLSN - 1, nil
	}
	for i := range frames {
		want := j.nextLSN + int64(i)
		if frames[i].LSN != want {
			return 0, fmt.Errorf("journal: non-consecutive LSN %d, want %d", frames[i].LSN, want)
		}
	}
	bufs := make([][]byte, 0, len(frames))
	var total int
	for i := range frames {
		var hdr [headerSize]byte
		binary.BigEndian.PutUint16(hdr[0:2], frameMagic)
		hdr[2] = byte(frames[i].Type)
		binary.BigEndian.PutUint64(hdr[3:11], uint64(frames[i].LSN))
		binary.BigEndian.PutUint32(hdr[11:15], uint32(len(frames[i].Payload)))
		crc := crc32.NewIEEE()
		crc.Write(hdr[2:15])
		crc.Write(frames[i].Payload)
		var foot [crcSize]byte
		binary.BigEndian.PutUint32(foot[0:4], crc.Sum32())
		bufs = append(bufs, hdr[:], frames[i].Payload, foot[:])
		total += headerSize + len(frames[i].Payload) + crcSize
	}
	buf := make([]byte, 0, total)
	for _, b := range bufs {
		buf = append(buf, b...)
	}
	if _, err := j.f.Write(buf); err != nil {
		return 0, err
	}
	if err := j.f.Sync(); err != nil {
		return 0, err
	}
	j.nextLSN += int64(len(frames))
	return j.nextLSN - 1, nil
}

// appendBatch assigns LSNs, encodes all frames into one buffer, performs a
// single write(2) and fsyncs. Callers must not mutate frames meanwhile.
func (j *Journal) appendBatch(frames []journalFrame) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(frames) == 0 {
		return j.nextLSN - 1, nil
	}
	bufs := make([][]byte, 0, len(frames))
	var total int
	lsn := j.nextLSN
	for i := range frames {
		frames[i].LSN = lsn
		var hdr [headerSize]byte
		binary.BigEndian.PutUint16(hdr[0:2], frameMagic)
		hdr[2] = byte(frames[i].Type)
		binary.BigEndian.PutUint64(hdr[3:11], uint64(lsn))
		binary.BigEndian.PutUint32(hdr[11:15], uint32(len(frames[i].Payload)))
		crc := crc32.NewIEEE()
		crc.Write(hdr[2:15])
		crc.Write(frames[i].Payload)
		var foot [crcSize]byte
		binary.BigEndian.PutUint32(foot[0:4], crc.Sum32())
		bufs = append(bufs, hdr[:], frames[i].Payload, foot[:])
		total += headerSize + len(frames[i].Payload) + crcSize
		lsn++
	}
	buf := make([]byte, 0, total)
	for _, b := range bufs {
		buf = append(buf, b...)
	}
	if _, err := j.f.Write(buf); err != nil {
		return 0, err
	}
	if err := j.f.Sync(); err != nil {
		return 0, err
	}
	j.nextLSN = lsn
	return lsn - 1, nil
}

func (j *Journal) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Close()
}

// ---- small JSON helpers ----

func marshalFrame(t frameType, v any) (journalFrame, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return journalFrame{}, err
	}
	return journalFrame{Type: t, Payload: b}, nil
}

func unmarshalFrame[T any](f journalFrame) (T, error) {
	var v T
	if err := json.Unmarshal(f.Payload, &v); err != nil {
		return v, err
	}
	return v, nil
}
