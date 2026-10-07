package periodclose

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const snapshotName = "snapshot.json"

// persistedPeriod is the on-disk shape of one period's derived state.
type persistedPeriod struct {
	Status      PeriodStatus
	Entries     []StoredEntry // entries attributed to the period
	Adjustments []Adjustment  // accepted corrections (reopen cycles accrue)
	Current     *CloseVersion // nil unless closed
	History     []CloseVersion
	CloseSeq    int64              // period-local close version counter
	Intent      *closeStartPayload // non-nil only while status == closing
}

// snapshot is the persisted derived state. The journal remains the source
// of truth; the snapshot is a rebuild checkpoint.
type snapshot struct {
	LastLSN    int64
	SeenEntry  map[string]int64 // entry id -> global seq (dedup index)
	SeenAdjust map[string]int64 // adjustment id -> global seq
	Rules      []Rule
	Periods    map[Period]*persistedPeriod
	AuditSeq   int64
}

func newSnapshot() *snapshot {
	return &snapshot{
		SeenEntry:  map[string]int64{},
		SeenAdjust: map[string]int64{},
		Periods:    map[Period]*persistedPeriod{},
	}
}

func snapshotPath(dir string) string { return filepath.Join(dir, snapshotName) }

// saveSnapshot writes atomically: temp file + fsync + rename, so readers
// never observe a partially written snapshot.
func saveSnapshot(dir string, snap *snapshot) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := snapshotPath(dir) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, snapshotPath(dir)); err != nil {
		return err
	}
	// fsync the directory so the rename itself is durable.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// loadSnapshot returns (nil,false,nil) when no snapshot exists yet.
func loadSnapshot(dir string) (*snapshot, bool, error) {
	b, err := os.ReadFile(snapshotPath(dir))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var snap snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, false, err
	}
	if snap.SeenEntry == nil {
		snap.SeenEntry = map[string]int64{}
	}
	if snap.SeenAdjust == nil {
		snap.SeenAdjust = map[string]int64{}
	}
	if snap.Periods == nil {
		snap.Periods = map[Period]*persistedPeriod{}
	}
	return &snap, true, nil
}
