//go:build darwin

package main

import (
	"os"
	"syscall"
	"time"
)

func fileAccessTime(info os.FileInfo) (time.Time, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return time.Time{}, false
	}

	return time.Unix(0, stat.Atimespec.Nano()), true
}
