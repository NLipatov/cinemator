package torrent

import (
	"context"
	"os"
	"time"

	"cinemator/media"

	"github.com/anacrolix/torrent/metainfo"
)

func (m *Manager) preparationProgress(ctx context.Context, download Download) *PreparationProgress {
	index := *download.SelectedFileIndex
	progress := &PreparationProgress{FileIndex: index, Stage: "analyzing", SampledAt: time.Now().UTC()}
	for _, file := range download.Files {
		if file.Index == index {
			progress.TotalBytes = file.Size
			break
		}
	}
	if m.client != nil {
		if t, ok := m.client.Torrent(metainfo.NewHashFromHex(download.ID)); ok {
			stats := t.Stats()
			progress.ReceivedBytes = stats.BytesReadUsefulData.Int64()
			progress.Peers = stats.ActivePeers
			select {
			case <-t.GotInfo():
				files := t.Files()
				if index >= 0 && index < len(files) {
					progress.DownloadedBytes = files[index].BytesCompleted()
				}
			default:
			}
		}
	}
	if _, cached, _ := m.downloads.loadMediaInfo(ctx, download.ID, index); cached {
		progress.Stage = "preparing"
	}
	paths := (streamKey{InfoHash: download.ID, Index: index, Audio: -1, Subtitle: -1}).paths(m.cfg.HLSPath)
	if data, err := os.ReadFile(paths.videoPlaylist); err == nil {
		progress.PreparedSeconds, _ = media.PlaylistDuration(data)
	}
	if _, err := os.Stat(paths.masterPlaylist); err == nil {
		progress.Stage = "playable"
	}
	return progress
}
