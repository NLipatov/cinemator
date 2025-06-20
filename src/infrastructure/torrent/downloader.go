package torrent

import (
	"cinemator/application"
	"cinemator/domain"
	"cinemator/domain/primitives"
	"cinemator/presentation/settings"
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/anacrolix/torrent"
	"golang.org/x/sync/errgroup"
)

type Downloader struct {
	client           *torrent.Client
	settings         settings.Settings
	magnetTorrentMap *primitives.TypedSyncMap[string, *torrent.Torrent]
}

func NewDownloader(settings settings.Settings) (application.Downloader, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = settings.DownloadPath()
	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	var mkdirErrGroup errgroup.Group
	mkdirErrGroup.Go(func() error {
		return os.MkdirAll(settings.HlsPath(), 0755)
	})
	mkdirErrGroup.Go(func() error {
		return os.MkdirAll(settings.DownloadPath(), 0755)
	})
	if mkdirAllErrs := mkdirErrGroup.Wait(); mkdirAllErrs != nil {
		return nil, mkdirAllErrs
	}

	m := &Downloader{
		client:           client,
		settings:         settings,
		magnetTorrentMap: primitives.NewTypedSyncMap[string, *torrent.Torrent](),
	}
	return m, nil
}

func (m *Downloader) ListFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error) {
	t, err := m.getOrCreateTorrentFromMagnet(magnet)
	if err != nil {
		return nil, err
	}

	timer := time.NewTimer(m.settings.TorrentInfoLookupDeadline())
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("torrent info lookup deadline exceeded")
	case <-t.GotInfo():
		files := t.Files()
		out := make([]domain.FileInfo, len(files))
		for i, f := range files {
			out[i] = domain.FileInfo{Index: i, Name: f.DisplayPath(), Size: f.Length()}
		}
		return out, nil
	}
}

func (m *Downloader) InfoHash(ctx context.Context, magnet string) (string, error) {
	t, err := m.getOrCreateTorrentFromMagnet(magnet)
	if err != nil {
		log.Printf("could not add magnet link: %v", err)
		return "", err
	}

	timer := time.NewTimer(m.settings.TorrentInfoLookupDeadline())
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", fmt.Errorf("torrent info lookup deadline exceeded")
	case <-t.GotInfo():
		return t.InfoHash().String(), nil
	}
}

func (m *Downloader) DownloadFile(ctx context.Context, magnet string, fileIndex int) (domain.ReadableFile, error) {
	t, err := m.getOrCreateTorrentFromMagnet(magnet)
	if err != nil {
		log.Printf("could not add magnet link: %v", err)
		return nil, err
	}

	timer := time.NewTimer(m.settings.TorrentInfoLookupDeadline())
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("torrent info lookup deadline exceeded")
	case <-t.GotInfo():
		files := t.Files()
		if fileIndex < 0 || fileIndex >= len(files) {
			log.Printf("DownloadFile failed: bad file index: %d", fileIndex)
			return nil, fmt.Errorf("bad file index")
		}

		fileToDownload := files[fileIndex]
		fileToDownload.Download()
		readableFile := NewReadableFile(fileToDownload)
		return readableFile, nil
	}
}

func (m *Downloader) getOrCreateTorrentFromMagnet(magnet string) (*torrent.Torrent, error) {
	storedTorrent, found := m.magnetTorrentMap.Load(magnet)
	if found {
		return storedTorrent, nil
	}

	newTorrent, addMagnetErr := m.client.AddMagnet(magnet)
	if addMagnetErr != nil {
		log.Printf("could not add magnet link: %v", addMagnetErr)
		return nil, addMagnetErr
	}

	m.magnetTorrentMap.Store(magnet, newTorrent)
	return newTorrent, nil
}
