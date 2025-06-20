package torrent

import (
	"cinemator/presentation/settings"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"cinemator/application"
	"cinemator/domain"

	"github.com/anacrolix/torrent"
)

type manager struct {
	client   *torrent.Client
	settings settings.Settings
}

func NewManager(settings settings.Settings) (application.TorrentManager, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = settings.DownloadPath()
	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	_ = os.MkdirAll(settings.HlsPath(), 0755)
	_ = os.MkdirAll(settings.DownloadPath(), 0755)
	m := &manager{
		client:   client,
		settings: settings,
	}
	return m, nil
}

func (m *manager) GetTorrentFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error) {
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		return nil, err
	}

	done := make(chan []domain.FileInfo, 1)
	go func() {
		<-t.GotInfo()
		files := t.Files()
		out := make([]domain.FileInfo, len(files))
		for i, f := range files {
			out[i] = domain.FileInfo{Index: i, Name: f.DisplayPath(), Size: f.Length()}
		}

		// prevents the goroutine from hanging if ctx is cancelled
		select {
		case done <- out:
		case <-ctx.Done():
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-done:
		return res, nil
	}
}

func (m *manager) PrepareHlsStream(ctx context.Context, magnet string, fileIndex int) (*torrent.File, string, string, context.CancelFunc, error) {
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		log.Printf("PrepareHlsStream: AddMagnet failed: %v", err)
		return nil, "", "", nil, err
	}
	<-t.GotInfo()
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		log.Printf("PrepareHlsStream: bad file index: %d", fileIndex)
		return nil, "", "", nil, fmt.Errorf("bad file index")
	}
	f := files[fileIndex]
	hash := t.InfoHash().HexString()
	outDir := filepath.Join(m.settings.HlsPath(), fmt.Sprintf("%s_%d", hash, fileIndex))
	playlist := filepath.Join(outDir, "index.m3u8")

	_ = os.MkdirAll(outDir, 0755)
	f.Download()
	ctx, cancel := context.WithCancel(ctx)
	return f, playlist, outDir, cancel, nil
}
