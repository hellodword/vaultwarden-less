package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
)

type Entry struct {
	Kind  string
	Size  int64
	Hash  string
	Link  string
	stamp os.FileInfo
}

func (e Entry) equal(other Entry) bool {
	return e.Kind == other.Kind && e.Size == other.Size && e.Hash == other.Hash && e.Link == other.Link
}

type fileCapture struct {
	ctx         context.Context
	destination string
	previous    *Payload
	readAll     bool
	entries     map[string]Entry
	issues      map[string]int
	afterRead   func(string)
}

func excludedPath(path string, mode os.FileMode) bool {
	switch path {
	case "db.sqlite3", "db.sqlite3-wal", "db.sqlite3-shm", "db.sqlite3-journal":
		return true
	case "icon_cache", "tmp":
		return mode.IsDir()
	}
	return false
}

// Open directory descriptors pin the traversal to real directories. We never
// follow links, including a directory replaced by a symlink during traversal.
func openAt(dir *os.File, name string, directory bool) (*os.File, error) {
	flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if directory {
		flags |= syscall.O_DIRECTORY
	}
	fd, err := syscall.Openat(int(dir.Fd()), name, flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func descriptorPath(dir *os.File, name string) string {
	return filepath.Join("/proc/self/fd", strconv.FormatUint(uint64(dir.Fd()), 10), name)
}

func sameVersion(left, right os.FileInfo) bool {
	if left == nil || right == nil || left.Mode() != right.Mode() || !os.SameFile(left, right) || left.Size() != right.Size() || left.ModTime() != right.ModTime() {
		return false
	}
	a, aOK := left.Sys().(*syscall.Stat_t)
	b, bOK := right.Sys().(*syscall.Stat_t)
	return aOK && bOK && a.Ctim == b.Ctim
}

func (c *fileCapture) walk(dir *os.File, prefix string) error {
	if err := c.ctx.Err(); err != nil {
		c.issues["capture_timeout"]++
		return nil
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		c.issues["directory_unreadable"]++
		return nil
	}
	sort.Strings(names)
	for _, name := range names {
		if c.ctx.Err() != nil {
			c.issues["capture_timeout"]++
			return nil
		}
		relative := filepath.Join(prefix, name)
		path := descriptorPath(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			c.issues["file_unavailable"]++
			continue
		}
		if excludedPath(relative, info.Mode()) {
			continue
		}
		destination := filepath.Join(c.destination, relative)
		switch {
		case info.IsDir():
			child, err := openAt(dir, name, true)
			if err != nil {
				c.issues["directory_unreadable"]++
				continue
			}
			opened, statErr := child.Stat()
			if statErr != nil || !os.SameFile(info, opened) {
				child.Close()
				c.issues["directory_changed"]++
				continue
			}
			if err := os.Mkdir(destination, 0o700); err != nil {
				child.Close()
				return errors.New("create payload directory")
			}
			c.entries[relative] = Entry{Kind: "directory"}
			err = c.walk(child, relative)
			child.Close()
			if err != nil {
				return err
			}
			after, statErr := os.Lstat(path)
			if statErr != nil || !sameVersion(info, after) {
				c.issues["directory_changed"]++
			}
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				c.issues["link_unreadable"]++
				continue
			}
			after, err := os.Lstat(path)
			if err != nil || !sameVersion(info, after) {
				c.issues["link_changed"]++
				continue
			}
			if err := os.Symlink(link, destination); err != nil {
				return errors.New("save payload symlink")
			}
			c.entries[relative] = Entry{Kind: "symlink", Link: link}
		case info.Mode().IsRegular():
			entry, sourceErr, err := c.copy(dir, name, relative, info)
			if err != nil {
				return err
			}
			if sourceErr != "" {
				c.issues[sourceErr]++
				continue
			}
			c.entries[relative] = entry
		default:
			c.issues["unsupported_file"]++
		}
	}
	return nil
}

// Only completed payload files are linked. Source files are always read into a
// new file. File metadata merely controls reads; it never enters equality.
func (c *fileCapture) copy(dir *os.File, name, relative string, info os.FileInfo) (Entry, string, error) {
	destination := filepath.Join(c.destination, relative)
	if !c.readAll && c.previous != nil {
		if old, ok := c.previous.Files[relative]; ok && old.Kind == "file" && old.stamp != nil && sameVersion(info, old.stamp) {
			if err := os.Link(filepath.Join(c.previous.Directory, relative), destination); err != nil {
				return Entry{}, "", errors.New("link completed payload file")
			}
			after, err := os.Lstat(descriptorPath(dir, name))
			if err != nil || !sameVersion(info, after) {
				os.Remove(destination)
				return Entry{}, "file_changed", nil
			}
			return old, "", nil
		}
	}
	source, err := openAt(dir, name, false)
	if err != nil {
		return Entry{}, "file_unreadable", nil
	}
	defer source.Close()
	opened, err := source.Stat()
	if err != nil || !opened.Mode().IsRegular() || !sameVersion(info, opened) {
		return Entry{}, "file_changed", nil
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Entry{}, "", errors.New("create payload file")
	}
	keep := false
	defer func() {
		output.Close()
		if !keep {
			os.Remove(destination)
		}
	}()
	hash := sha256.New()
	var copied int64
	buffer := make([]byte, 256<<10)
	for {
		if c.ctx.Err() != nil {
			return Entry{}, "capture_timeout", nil
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			written, writeErr := output.Write(buffer[:n])
			if writeErr != nil || written != n {
				return Entry{}, "", errors.New("write payload file")
			}
			hash.Write(buffer[:n])
			copied += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return Entry{}, "file_unreadable", nil
		}
	}
	if c.afterRead != nil {
		c.afterRead(relative)
	}
	after, err := source.Stat()
	latest, latestErr := os.Lstat(descriptorPath(dir, name))
	if err != nil || latestErr != nil || copied != info.Size() || !sameVersion(info, after) || !sameVersion(info, latest) {
		return Entry{}, "file_changed", nil
	}
	if err := output.Sync(); err != nil {
		return Entry{}, "", errors.New("sync payload file")
	}
	if err := output.Close(); err != nil {
		return Entry{}, "", errors.New("close payload file")
	}
	keep = true
	return Entry{Kind: "file", Size: copied, Hash: hex.EncodeToString(hash.Sum(nil)), stamp: info}, "", nil
}

func hashDatabase(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("open copied database")
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buffer)
		hash.Write(buffer[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read copied database")
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
