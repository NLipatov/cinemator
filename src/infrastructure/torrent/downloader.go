package torrent

import (
	"cinemator/application"
	"cinemator/domain"
	"cinemator/presentation/settings"
	"context"
	"fmt"
	"log"
	"os"

	"github.com/anacrolix/torrent"
)

type TorrentDownloader struct {
	client   *torrent.Client
	settings settings.Settings
}

func NewDownloader(settings settings.Settings) (application.TorrentManager, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = settings.DownloadPath()
	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	_ = os.MkdirAll(settings.HlsPath(), 0755)
	_ = os.MkdirAll(settings.DownloadPath(), 0755)
	m := &TorrentDownloader{
		client:   client,
		settings: settings,
	}
	return m, nil
}

func (m *TorrentDownloader) ListFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error) {
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.GotInfo():
		files := t.Files()
		out := make([]domain.FileInfo, len(files))
		for i, f := range files {
			out[i] = domain.FileInfo{Index: i, Name: f.DisplayPath(), Size: f.Length()}
		}
		return out, nil
	}
}

func (m *TorrentDownloader) InfoHash(ctx context.Context, magnet string) (string, error) {
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		log.Printf("could not add magnet link: %v", err)
		return "", err
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-t.GotInfo():
		return t.InfoHash().String(), nil
	}
}

func (m *TorrentDownloader) DownloadFile(ctx context.Context, magnet string, fileIndex int) (*torrent.File, error) {
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		log.Printf("could not add magnet link: %v", err)
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.GotInfo():
		files := t.Files()
		if fileIndex < 0 || fileIndex >= len(files) {
			log.Printf("DownloadFile failed: bad file index: %d", fileIndex)
			return nil, fmt.Errorf("bad file index")
		}

		fileToDownload := files[fileIndex]
		fileToDownload.Download()
		return fileToDownload, nil
	}
}
