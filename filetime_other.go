//go:build !darwin && !linux && !windows

package main

import (
	"os"
	"time"
)

func fileAccessTime(info os.FileInfo) (time.Time, bool) {
	return time.Time{}, false
}
