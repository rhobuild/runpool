// Package atomicfile replaces a file so that no reader can observe it
// partly written.
//
// It exists because two very different parts of the product need the
// same guarantee against the same mistake. The egress gateway's policy
// is installed by one process and read by another with no lock between
// them; the capsule supervisor's control surface is written inside a
// container and read from outside it by `docker exec`. In both, the
// writer's obvious call — os.WriteFile — creates and truncates before it
// writes, so a read landing in that window succeeds and returns nothing.
// Nothing is not "not yet": it is a different answer, and both readers
// act on it.
//
// Keeping one copy is the point. There were two, and they had already
// drifted apart on the details that matter.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Replace writes data to a temporary file beside path and renames it
// over the top. A reader sees the file before or the file after, never
// one mid-write.
//
// The temporary name is unique. Two writers sharing one would truncate
// and fill the same file in turn, which puts the window back one level
// down and publishes whichever interleaving won.
//
// uid and gid are applied before the rename, so a file that needs an
// owner never appears without it. They are applied after the contents,
// so it never appears to that owner empty either. -1 leaves the owner
// alone, as os.Chown does.
func Replace(path string, data []byte, perm os.FileMode, uid, gid int) error {
	if err := replace(path, data, perm, uid, gid); err != nil {
		// The temporary name is an implementation detail, and it is gone
		// by the time anyone reads the error. Naming the target is what
		// a caller can act on — and in the capsule this text lands in
		// the state file an operator reads.
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func replace(path string, data []byte, perm os.FileMode, uid, gid int) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	// Only until the rename succeeds. Removing afterwards would delete a
	// name that is free again and may already belong to another writer's
	// temporary file, whose own rename would then fail for no reason it
	// could report.
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmp.Name())
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if uid >= 0 || gid >= 0 {
		if err := tmp.Chown(uid, gid); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	renamed = true
	return nil
}
