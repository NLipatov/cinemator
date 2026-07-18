//go:build !unix

package torrent

import "errors"

func filesystemAvailability(string) (diskAvailability, error) {
	return diskAvailability{}, errors.New("filesystem admission is unsupported on this platform")
}

func filesystemDevice(path string) (string, error) { return path, nil }
