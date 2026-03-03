//go:build windows
// +build windows

package config

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockFile(f *os.File) error {
	h := windows.Handle(f.Fd())
	var ol windows.Overlapped

	// Lock a huge region starting at offset 0 to effectively cover the whole file.
	return windows.LockFileEx(
		h,
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		0xFFFFFFFF,
		0xFFFFFFFF,
		&ol,
	)
}

func unlockFile(f *os.File) error {
	h := windows.Handle(f.Fd())
	var ol windows.Overlapped

	return windows.UnlockFileEx(
		h,
		0,
		0xFFFFFFFF,
		0xFFFFFFFF,
		&ol,
	)
}
