package compliance

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditLog 是只追加的审计日志。
//
// 持久化：DataDir/audit.log，每行一个 JSON 对象。
// 并发：单一互斥串行化追加，追加成功后 fsync（配置开启时），
// 因此并发写入不会互相破坏或丢失。审计文件从不做原地修改。
type AuditLog struct {
	mu   sync.Mutex
	f    *os.File
	seq  int64
	evs  []AuditEvent
	path string
}

// NewAuditLog 创建纯内存审计日志。
func NewAuditLog() *AuditLog {
	return &AuditLog{}
}

// OpenAuditLog 打开（必要时创建）目录 dir 下的审计日志并回放已有记录。
func OpenAuditLog(dir string) (*AuditLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "audit.log")
	l := &AuditLog{path: path}

	// 回放已有记录：逐行解码，最后一行若是半截写则截断掉。
	if existing, err := os.ReadFile(path); err == nil && len(existing) > 0 {
		good := l.replay(existing)
		if good < len(existing) {
			if err := os.Truncate(path, int64(good)); err != nil {
				return nil, err
			}
		}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	l.f = f
	return l, nil
}

// replay 解析 buf 中完整的 JSON 行，返回最后一条完整记录结束的字节位置。
func (l *AuditLog) replay(buf []byte) int {
	pos := 0
	start := 0
	for i := 0; i <= len(buf); i++ {
		if i == len(buf) || buf[i] == '\n' {
			line := buf[start:i]
			if len(line) > 0 {
				var ev AuditEvent
				if err := json.Unmarshal(line, &ev); err != nil || ev.Seq <= 0 {
					return pos // 半截/损坏行：丢弃本行及之后内容
				}
				l.evs = append(l.evs, ev)
				if ev.Seq > l.seq {
					l.seq = ev.Seq
				}
				pos = i + 1
			}
			start = i + 1
		}
	}
	return pos
}

// Append 追加审计记录，自动分配单调序号；并发安全、持久化后才返回。
func (l *AuditLog) Append(ev AuditEvent) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	ev.Seq = l.seq
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		l.seq--
		return 0, err
	}
	data = append(data, '\n')
	if l.f != nil {
		if _, err := l.f.Write(data); err != nil {
			l.seq--
			return 0, err
		}
		if err := l.f.Sync(); err != nil {
			l.seq--
			return 0, err
		}
	}
	l.evs = append(l.evs, ev)
	return ev.Seq, nil
}

// List 返回全部审计记录的副本快照。
func (l *AuditLog) List() []AuditEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]AuditEvent, len(l.evs))
	copy(out, l.evs)
	return out
}

// Since 返回 seq 之后（不含 seq）的审计记录。
func (l *AuditLog) Since(seq int64) []AuditEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]AuditEvent, 0)
	for _, ev := range l.evs {
		if ev.Seq > seq {
			out = append(out, ev)
		}
	}
	return out
}

// Close 关闭底层文件。
func (l *AuditLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		err := l.f.Sync()
		if c := l.f.Close(); err == nil {
			err = c
		}
		l.f = nil
		return err
	}
	return nil
}

var _ io.Closer = (*AuditLog)(nil)
