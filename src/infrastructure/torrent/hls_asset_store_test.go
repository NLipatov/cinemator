package torrent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestHlsAssetStoreDoesNotUnlinkLeasedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "stream", "chunk.ts")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("segment"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := store.TryEvict(path); err != nil || removed {
		t.Fatalf("TryEvict() = %t, %v; want busy", removed, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("leased file lost its directory entry: %v", err)
	}
	data, err := io.ReadAll(asset)
	if err != nil || string(data) != "segment" {
		t.Fatalf("read leased file = %q, %v", data, err)
	}
	if err := asset.Close(); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.TryEvict(path); err != nil || !removed {
		t.Fatalf("TryEvict() after close = %t, %v", removed, err)
	}
}

func TestHlsAssetStoreRetiresTreeAfterLastReaderCloses(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "stream")
	path := filepath.Join(dir, "chunk.ts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("segment"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RetireTree(dir); !errors.Is(err, errHlsAssetsBusy) {
		t.Fatalf("RetireTree() error = %v, want busy", err)
	}
	if _, err := store.Open(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open() after retirement error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("retired leased file was unlinked: %v", err)
	}
	if err := asset.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file remains after final close: %v", err)
	}
}

func TestHlsAssetStoreRemovesRetiredTreeWithEmptySiblingDirsAfterLastClose(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "stream")
	leased := filepath.Join(dir, "leased", "chunk.ts")
	stale := filepath.Join(dir, "stale", "nested", "chunk.ts")
	for _, path := range []string{leased, stale} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("segment"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := store.Open(leased)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RetireTree(dir); !errors.Is(err, errHlsAssetsBusy) {
		t.Fatalf("RetireTree() error = %v, want busy", err)
	}
	if err := asset.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired tree remains after final close: %v", err)
	}
}

func TestHlsAssetStoreDoesNotFollowAncestorOutsideRoot(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	victim := filepath.Join(external, "victim.ts")
	if err := os.WriteFile(victim, []byte("external"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "stream")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(root, "stream", "victim.ts")
	if _, err := store.Open(path); err == nil {
		t.Fatal("asset through an external symlink ancestor was opened")
	}
	if removed, err := store.TryEvict(path); err == nil || removed {
		t.Fatalf("external asset eviction = %t, %v", removed, err)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "external" {
		t.Fatalf("external file changed: %q, %v", data, err)
	}
}

func TestHlsAssetStoreStaysBoundToOriginalRootAfterPathReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "cache")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	store, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	victim := filepath.Join(external, "victim.ts")
	if err := os.WriteFile(victim, []byte("external"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(root, "victim.ts")
	if _, err := store.Open(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open() after root replacement error = %v", err)
	}
	if removed, err := store.TryEvict(path); err != nil || !removed {
		t.Fatalf("TryEvict() after root replacement = %t, %v", removed, err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("external file was removed: %v", err)
	}
}

func TestHlsAssetStoreRejectsSymlinkAndHardLinkEviction(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.ts")
	if err := os.WriteFile(target, []byte("segment"), 0644); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "symlink.ts")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	store, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(symlink); err == nil {
		t.Fatal("symbolic-link asset was opened")
	}
	if removed, err := store.TryEvict(symlink); err == nil || removed {
		t.Fatalf("symbolic-link asset eviction = %t, %v", removed, err)
	}
	hardlink := filepath.Join(root, "hardlink.ts")
	if err := os.Link(target, hardlink); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.TryEvict(target); err == nil || removed {
		t.Fatalf("hard-linked asset eviction = %t, %v", removed, err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("hard-linked asset was removed: %v", err)
	}
}
