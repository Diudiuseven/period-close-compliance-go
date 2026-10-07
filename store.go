package compliance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// WAL 事件类型。所有状态变更都先写 WAL 再更新内存。
const (
	evEntry       = "entry"
	evAdjustment  = "adjustment"
	evRule        = "rule"
	evCloseStart  = "close_start"
	evCloseCommit = "close_commit"
	evCloseAbort  = "close_abort"
	evReopen      = "reopen"
)

// rec 是预写日志中的一条记录。
type rec struct {
	Seq  int64           `json:"seq"`
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body"`
}

// wal 是只追加的预写日志（每行一个 JSON）。
// 全局只有一个追加互斥：它只保护"编码+写行+fsync"这段很短的 IO，
// 慢期间结账持有的是自己的期间锁，不会阻塞其他期间的明细摄入。
type wal struct {
	mu    sync.Mutex
	dir   string
	f     *os.File
	seq   int64
	fsync bool
}

// openWAL 打开/创建 WAL 并回放已有记录确定序号；半截尾行会被截断。
func openWAL(dir string, fsync bool) (*wal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "wal.log")
	w := &wal{dir: dir, fsync: fsync}

	if existing, err := os.ReadFile(path); err == nil && len(existing) > 0 {
		good, maxSeq, err := scanRecs(existing)
		if err != nil {
			return nil, err
		}
		if good < len(existing) {
			if err := os.Truncate(path, int64(good)); err != nil {
				return nil, err
			}
		}
		w.seq = maxSeq
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	w.f = f
	return w, nil
}

// scanRecs 逐行解析，返回最后一条完整记录结束位置与最大序号。
// 遇到半截/损坏行即停止（该行及之后内容在打开时被截断丢弃）。
func scanRecs(buf []byte) (int, int64, error) {
	pos := 0
	start := 0
	var maxSeq int64
	for i := 0; i <= len(buf); i++ {
		if i == len(buf) || buf[i] == '\n' {
			line := buf[start:i]
			if len(line) > 0 {
				var r rec
				if err := json.Unmarshal(line, &r); err != nil || r.Seq <= 0 {
					return pos, maxSeq, nil // 尾行半截：保留此前全部完整记录
				}
				if r.Seq != maxSeq+1 {
					// 序号不连续说明日志被外部破坏：拒绝继续，避免跳过/重复。
					return 0, 0, ErrInvalidArgument
				}
				maxSeq = r.Seq
				pos = i + 1
			}
			start = i + 1
		}
	}
	return pos, maxSeq, nil
}

// append 串行化写入一条记录并在 fsync 成功后才返回其序号。
func (w *wal) append(kind string, v any) (rec, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return rec{}, err
	}
	data, err := json.Marshal(rec{Seq: w.seq + 1, Kind: kind, Body: body})
	if err != nil {
		return rec{}, err
	}
	data = append(data, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Write(data); err != nil {
		return rec{}, err
	}
	if w.fsync {
		if err := w.f.Sync(); err != nil {
			return rec{}, err
		}
	}
	w.seq++
	return rec{Seq: w.seq, Kind: kind, Body: body}, nil
}

// readAll 读取并解析 WAL 中的全部完整记录。
func (w *wal) readAll() ([]rec, error) {
	buf, err := os.ReadFile(filepath.Join(w.dir, "wal.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []rec
	start := 0
	for i := 0; i <= len(buf); i++ {
		if i == len(buf) || buf[i] == '\n' {
			if i > start {
				var r rec
				if err := json.Unmarshal(buf[start:i], &r); err != nil {
					break
				}
				out = append(out, r)
			}
			start = i + 1
		}
	}
	return out, nil
}

func (w *wal) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		err := w.f.Sync()
		if c := w.f.Close(); err == nil {
			err = c
		}
		w.f = nil
		return err
	}
	return nil
}

// snapshot 是最近一次已提交状态的原子快照。
type snapshot struct {
	LastSeq int64 `json:"last_seq"`
	State   any   `json:"state"`
}

// writeSnapshot 通过"临时文件 + rename"原子替换快照文件，
// 读取方永远不会看到写了一半的快照。
func writeSnapshot(dir string, s snapshot) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "snapshot.json.tmp")
	final := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// readSnapshot 读取快照；不存在返回 (nil, nil)。
func readSnapshot(dir string) (*snapshot, error) {
	data, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
