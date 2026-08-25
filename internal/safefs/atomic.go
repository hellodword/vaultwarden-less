package safefs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// PublishState describes how far an atomic replacement got. A rename is the
// publication boundary: once it has happened, callers must use the new value
// even if syncing its parent directory could not be completed.
type PublishState string

const (
	Durable              PublishState = "durable"
	Unpublished          PublishState = "unpublished"
	PublishedUnconfirmed PublishState = "published_unconfirmed"
)

// FileOps is deliberately narrow so persistence tests can inject one failure
// at each filesystem boundary without creating a repository-wide mock layer.
// A zero field uses the corresponding os.File operation.
type FileOps struct {
	CreateTemp func(string, string) (*os.File, error)
	Chmod      func(*os.File, fs.FileMode) error
	Write      func(*os.File, []byte) (int, error)
	SyncFile   func(*os.File) error
	Close      func(*os.File) error
	Rename     func(string, string) error
	Remove     func(string) error
	OpenDir    func(string) (*os.File, error)
	SyncDir    func(*os.File) error
}

func defaultFileOps() FileOps {
	return FileOps{
		CreateTemp: os.CreateTemp,
		Chmod:      func(file *os.File, mode fs.FileMode) error { return file.Chmod(mode) },
		Write:      func(file *os.File, data []byte) (int, error) { return file.Write(data) },
		SyncFile:   func(file *os.File) error { return file.Sync() },
		Close:      func(file *os.File) error { return file.Close() },
		Rename:     os.Rename,
		Remove:     os.Remove,
		OpenDir:    os.Open,
		SyncDir:    func(file *os.File) error { return file.Sync() },
	}
}

func (ops FileOps) withDefaults() FileOps {
	defaults := defaultFileOps()
	if ops.CreateTemp == nil {
		ops.CreateTemp = defaults.CreateTemp
	}
	if ops.Chmod == nil {
		ops.Chmod = defaults.Chmod
	}
	if ops.Write == nil {
		ops.Write = defaults.Write
	}
	if ops.SyncFile == nil {
		ops.SyncFile = defaults.SyncFile
	}
	if ops.Close == nil {
		ops.Close = defaults.Close
	}
	if ops.Rename == nil {
		ops.Rename = defaults.Rename
	}
	if ops.Remove == nil {
		ops.Remove = defaults.Remove
	}
	if ops.OpenDir == nil {
		ops.OpenDir = defaults.OpenDir
	}
	if ops.SyncDir == nil {
		ops.SyncDir = defaults.SyncDir
	}
	return ops
}

// SyncParent opens and fsyncs the parent directory of path.
func SyncParent(path string, ops FileOps) error {
	ops = ops.withDefaults()
	directory, err := ops.OpenDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := ops.SyncDir(directory)
	closeErr := ops.Close(directory)
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// AtomicReplace writes data to a same-directory 0600 temporary file, flushes
// it, renames it into place, and flushes the parent directory. Before rename,
// errors are unpublished and the old destination remains authoritative. After
// rename, a parent-sync error is published_unconfirmed and is never rolled
// back by this function.
func AtomicReplace(path string, data []byte, ops FileOps) (PublishState, error) {
	ops = ops.withDefaults()
	temp, err := ops.CreateTemp(filepath.Dir(path), ".atomic-")
	if err != nil {
		return Unpublished, fmt.Errorf("create atomic temporary file: %w", err)
	}
	tempPath := temp.Name()
	open := true
	removeTemp := func() {
		if tempPath != "" {
			_ = ops.Remove(tempPath)
		}
	}
	defer func() {
		if open {
			_ = ops.Close(temp)
		}
		if tempPath != "" {
			// Remove is harmless after a successful rename and lets injected
			// implementations observe only the actual temporary path.
			_ = ops.Remove(tempPath)
		}
	}()
	if err := ops.Chmod(temp, 0o600); err != nil {
		removeTemp()
		return Unpublished, fmt.Errorf("secure atomic temporary file: %w", err)
	}
	written, err := ops.Write(temp, data)
	if err != nil {
		removeTemp()
		return Unpublished, fmt.Errorf("write atomic temporary file: %w", err)
	}
	if written != len(data) {
		removeTemp()
		return Unpublished, fmt.Errorf("write atomic temporary file: short write")
	}
	if err := ops.SyncFile(temp); err != nil {
		removeTemp()
		return Unpublished, fmt.Errorf("sync atomic temporary file: %w", err)
	}
	if err := ops.Close(temp); err != nil {
		open = false
		removeTemp()
		return Unpublished, fmt.Errorf("close atomic temporary file: %w", err)
	}
	open = false
	if err := ops.Rename(tempPath, path); err != nil {
		removeTemp()
		return Unpublished, fmt.Errorf("publish atomic file: %w", err)
	}
	tempPath = ""
	if err := SyncParent(path, ops); err != nil {
		return PublishedUnconfirmed, fmt.Errorf("sync atomic file parent: %w", err)
	}
	return Durable, nil
}

// SameFileVersion identifies the same regular file contents boundary used by
// stable-copy and private secret readers. ctime catches in-place replacement
// on filesystems where size and mtime are preserved.
func SameFileVersion(left, right os.FileInfo) bool {
	if left == nil || right == nil || !left.Mode().IsRegular() || !right.Mode().IsRegular() ||
		!os.SameFile(left, right) || left.Size() != right.Size() || left.ModTime() != right.ModTime() {
		return false
	}
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && leftStat.Ctim == rightStat.Ctim
}
