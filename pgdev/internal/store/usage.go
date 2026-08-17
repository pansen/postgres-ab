package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// DiskBytes reports how much space a directory tree actually occupies, summing
// each file's ALLOCATED blocks rather than its apparent length. On this store
// that is the number that matters: PostgreSQL's segment files are sparse in
// places, and a reflink clone allocates nothing until something writes to it.
//
// Blocks shared with a reflinked snapshot are counted in full here, so the live
// data dir and every snapshot each report the blocks they reference. Summing
// them therefore exceeds what the filesystem reports as used — this answers
// "how big is this tree", not "how much would deleting it free".
//
// Entries that vanish mid-walk are skipped: this runs against a live data
// directory where PostgreSQL recycles WAL and temp files underneath it.
func DiskBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// A missing root is a real failure; a file that disappeared during
			// the walk is not (WalkDir reports both here).
			if errors.Is(err, os.ErrNotExist) && total > 0 {
				return nil
			}
			return err
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			total += info.Size() // no block counts on this platform: apparent size
			return nil
		}
		total += int64(st.Blocks) * 512 // st_blocks is always in 512-byte units
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
