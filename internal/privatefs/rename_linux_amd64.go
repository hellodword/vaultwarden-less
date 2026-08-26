package privatefs

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	linuxAMD64Renameat2 = 316
	renameNoReplaceFlag = 1
	renameExchangeFlag  = 2
)

// RenameNoReplace publishes oldPath only when newPath is absent. There is no
// check-then-rename fallback: an unsupported filesystem or kernel fails safe.
func RenameNoReplace(oldPath, newPath string) error {
	return renameat2(oldPath, newPath, renameNoReplaceFlag)
}

// RenameExchange atomically swaps two existing paths.
func RenameExchange(left, right string) error {
	return renameat2(left, right, renameExchangeFlag)
}

func renameat2(left, right string, flags uintptr) error {
	leftPointer, err := syscall.BytePtrFromString(left)
	if err != nil {
		return err
	}
	rightPointer, err := syscall.BytePtrFromString(right)
	if err != nil {
		return err
	}
	atFDCWD := ^uintptr(99) // Two's-complement representation of Linux AT_FDCWD (-100).
	_, _, callError := syscall.Syscall6(
		linuxAMD64Renameat2,
		atFDCWD,
		uintptr(unsafe.Pointer(leftPointer)),
		atFDCWD,
		uintptr(unsafe.Pointer(rightPointer)),
		flags,
		0,
	)
	if callError != 0 {
		return os.NewSyscallError("renameat2", callError)
	}
	return nil
}

// SyncDirectory makes preceding entry creation, rename, or removal durable.
func SyncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}
