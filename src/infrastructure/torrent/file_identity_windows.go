//go:build windows

package torrent

import (
	"os"

	"golang.org/x/sys/windows"
)

func platformFileLinkCount(path string, expected os.FileInfo) (uint64, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(expected, actual) {
		return 0, false
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return 0, false
	}
	return uint64(info.NumberOfLinks), true
}
