package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Record is the persisted intent for one in-flight task. It IS the transaction
// state (replacing the shell version's "filesystem path existence as the
// record"): on a fresh start, a surviving Record tells Recover exactly how to
// finish or unwind an interrupted mutation.
type Record struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Args      map[string]string `json:"args,omitempty"`
	Commit    int               `json:"commit"`
	Completed int               `json:"completed"`
}

// Builder rebuilds a Task's Steps/Ensures from persisted Args. ID/Kind/Args/
// Commit are re-applied by Recover, so a Builder only needs to reconstruct the
// callbacks.
type Builder func(args map[string]string) (Task, error)

// Registry maps a task Kind to its Builder.
type Registry map[string]Builder

// Journal persists task intent durably. All writes fsync before returning.
type Journal interface {
	Begin(rec Record) error
	Advance(id string, completed int) error
	Commit(id string) error // resolve: delete the record
	Pending() ([]Record, error)
}

// FileJournal stores one JSON record per task under Dir, named "<id>.json"
// (path separators in IDs are escaped). Writes are atomic (temp + rename) and
// fsync both the file and the directory.
type FileJournal struct {
	Dir string
}

// NewFileJournal ensures the journal directory exists.
func NewFileJournal(dir string) (*FileJournal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create journal dir: %w", err)
	}
	return &FileJournal{Dir: dir}, nil
}

func (f *FileJournal) path(id string) string {
	return filepath.Join(f.Dir, safeName(id)+".json")
}

func (f *FileJournal) Begin(rec Record) error {
	return f.write(rec)
}

func (f *FileJournal) Advance(id string, completed int) error {
	rec, err := f.read(id)
	if err != nil {
		return err
	}
	rec.Completed = completed
	return f.write(rec)
}

func (f *FileJournal) Commit(id string) error {
	if err := os.Remove(f.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsyncDir(f.Dir)
}

func (f *FileJournal) Pending() ([]Record, error) {
	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var recs []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(f.Dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var rec Record
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil, fmt.Errorf("corrupt journal record %s: %w", e.Name(), err)
		}
		recs = append(recs, rec)
	}
	// Deterministic order so recovery is reproducible.
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	return recs, nil
}

func (f *FileJournal) read(id string) (Record, error) {
	var rec Record
	b, err := os.ReadFile(f.path(id))
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(b, &rec)
}

func (f *FileJournal) write(rec Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := f.path(rec.ID) + ".tmp"
	fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(b); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path(rec.ID)); err != nil {
		return err
	}
	return fsyncDir(f.Dir)
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// safeName makes a task ID usable as a filename (IDs use ':' as a separator).
func safeName(id string) string {
	return strings.NewReplacer("/", "_", ":", "_", string(os.PathSeparator), "_").Replace(id)
}
