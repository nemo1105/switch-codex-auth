//go:build windows

package main

import (
	"os"
	"syscall"
	"time"
)

func fileAccessTime(info os.FileInfo) (time.Time, bool) {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || data == nil {
		return time.Time{}, false
	}

	return time.Unix(0, data.LastAccessTime.Nanoseconds()), true
}
