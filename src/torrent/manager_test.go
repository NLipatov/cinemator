package torrent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHLSDiskSizesAggregatesRenditionsByDownload(t *testing.T) {
	root := t.TempDir()
	firstID := strings.Repeat("a", 40)
	secondID := strings.Repeat("b", 40)
	want := map[string]int64{firstID: 0, secondID: 0}

	files := []struct {
		key  streamKey
		name string
		data string
	}{
		{key: streamKey{InfoHash: firstID, Index: 0, Audio: -1, Subtitle: -1}, name: "video.ts", data: "video"},
		{key: streamKey{InfoHash: firstID, Index: 1, Audio: -1, Subtitle: -1}, name: "audio.ts", data: "audio"},
		{key: streamKey{InfoHash: secondID, Index: 0, Audio: -1, Subtitle: -1}, name: "video.ts", data: "second"},
	}
	for _, file := range files {
		dir := filepath.Join(root, file.key.dirName())
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, file.name)
		if err := os.WriteFile(path, []byte(file.data), 0644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want[file.key.InfoHash] += allocatedFileSize(info)
	}

	invalidDir := filepath.Join(root, "not-an-info-hash_0_a-1_s-1")
	if err := os.MkdirAll(invalidDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidDir, "ignored.ts"), []byte("ignored"), 0644); err != nil {
		t.Fatal(err)
	}

	if got := hlsDiskSizes(root); !reflect.DeepEqual(got, want) {
		t.Fatalf("hlsDiskSizes() = %v, want %v", got, want)
	}
	if got := hlsDiskSize(root, firstID); got != want[firstID] {
		t.Fatalf("hlsDiskSize() = %d, want %d", got, want[firstID])
	}
}
