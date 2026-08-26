package snapshot

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	linuxAMD64Renameat2 = 316
	renameExchangeFlag  = 2
)

// renameExchange atomically swaps two paths on the supported Linux/amd64
// product platform. Using the kernel primitive avoids a persistent previous
// name and keeps the last verified point authoritative on publication error.
func renameExchange(left, right string) error {
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
		renameExchangeFlag,
		0,
	)
	if callError != 0 {
		return os.NewSyscallError("renameat2", callError)
	}
	return nil
}
