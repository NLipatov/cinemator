package torrent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestTorrentStorageCloseReleasesDataFile(t *testing.T) {
	root := t.TempDir()
	clientStorage := storage.NewFileByInfoHash(root)
	t.Cleanup(func() {
		if err := clientStorage.Close(); err != nil {
			t.Errorf("close client storage: %v", err)
		}
	})

	info := metainfo.Info{
		Name:        "payload.bin",
		Length:      4096,
		PieceLength: 4096,
		Pieces:      make([]byte, metainfo.HashSize),
	}
	var infoHash metainfo.Hash
	infoHash[0] = 1
	torrentStorage, err := clientStorage.OpenTorrent(context.Background(), &info, infoHash)
	if err != nil {
		t.Fatalf("open torrent storage: %v", err)
	}

	piece := torrentStorage.Piece(info.Piece(0))
	if _, err := piece.WriteAt([]byte("data"), 0); err != nil {
		t.Fatalf("write torrent data: %v", err)
	}
	dataPath := findTorrentDataFile(t, root)
	if !fileDescriptorOpen(t, dataPath) {
		t.Skip("torrent storage is not using mmap file I/O")
	}

	if err := torrentStorage.Close(); err != nil {
		t.Fatalf("close torrent storage: %v", err)
	}
	if fileDescriptorOpen(t, dataPath) {
		t.Fatalf("torrent storage left %s open after Close", dataPath)
	}
}

func findTorrentDataFile(t *testing.T, root string) string {
	t.Helper()
	var dataPath string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.Contains(entry.Name(), "payload.bin") {
			dataPath = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("find torrent data file: %v", err)
	}
	if dataPath == "" {
		t.Fatal("torrent data file was not created")
	}
	return dataPath
}

func fileDescriptorOpen(t *testing.T, path string) bool {
	t.Helper()
	if runtime.GOOS != "linux" {
		lsof, err := exec.LookPath("lsof")
		if err != nil {
			t.Skip("file descriptor inspection is unavailable")
		}
		err = exec.Command(lsof, "-a", "-p", strconv.Itoa(os.Getpid()), path).Run()
		if err == nil {
			return true
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false
		}
		t.Fatalf("inspect file descriptors: %v", err)
	}

	want, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat torrent data file: %v", err)
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read file descriptors: %v", err)
	}
	for _, entry := range entries {
		got, err := os.Stat(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && os.SameFile(want, got) {
			return true
		}
	}
	return false
}
