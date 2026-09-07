package torrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	torrentlib "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestTerminalPreparationCleanupWaitsForCanceledWebseed(t *testing.T) {
	requested := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requested <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	clientConfig := torrentlib.TestingConfig(t)
	clientConfig.DefaultStorage = storage.NewFileByInfoHash(root)
	client, err := torrentlib.NewClient(clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	piece := bytes.Repeat([]byte{42}, 16<<10)
	hash := sha1.Sum(piece)
	info := metainfo.Info{Name: "movie.mp4", PieceLength: int64(len(piece)), Length: int64(len(piece)), Pieces: hash[:]}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	meta := metainfo.MetaInfo{InfoBytes: infoBytes, UrlList: []string{server.URL + "/"}}
	source, _, err := client.AddTorrentSpec(torrentlib.TorrentSpecFromMetaInfo(&meta))
	if err != nil {
		t.Fatal(err)
	}
	store, err := newDownloadStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id := meta.HashInfoBytes().HexString()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := store.upsert(ctx, id, "magnet:?xt=urn:btih:"+id, []FileInfo{{Index: 0, Name: info.Name, Size: info.Length}}); err != nil {
		t.Fatal(err)
	}
	if err := store.finishPreparation(ctx, id, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	source.DownloadAll()
	select {
	case <-requested:
	case <-ctx.Done():
		t.Fatal("webseed request did not start")
	}
	m := &Manager{client: client, downloads: store}
	if err := waitForDone(ctx, m.cleanupTransientPayload(id, source)); err != nil {
		t.Fatal(err)
	}
	if _, exists := client.Torrent(meta.HashInfoBytes()); exists {
		t.Fatal("completed preparation retained its torrent after canceling the webseed")
	}
	entries, err := os.ReadDir(store.downloadDir(id))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != downloadStoreDirName {
		t.Fatalf("cleanup retained payload: %v", entries)
	}
}
