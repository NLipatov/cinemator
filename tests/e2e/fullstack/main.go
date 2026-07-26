package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"

	"cinemator/presentation/settings"
	"cinemator/presentation/web/api"
)

const (
	appPort     = 4174
	controlPort = 4175
)

type fixtureState struct {
	Magnet   string  `json:"magnet"`
	FileName string  `json:"fileName"`
	Duration float64 `json:"duration"`
	Targets  []int   `json:"targets"`
}

func main() {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Fatal("ffmpeg is required for full-stack Playwright tests")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		log.Fatal("ffprobe is required for full-stack Playwright tests")
	}

	root, err := os.MkdirTemp("", "cinemator-playwright-fullstack-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(root)

	seedRoot := filepath.Join(root, "seed")
	if err := os.MkdirAll(seedRoot, 0o755); err != nil {
		log.Fatal(err)
	}
	source := makeMediaFixture(seedRoot)
	mi, info := makeMetainfo(source)
	seeder := startSeeder(seedRoot, mi)
	defer seeder.Close()

	magnet := mi.Magnet(nil, info)
	magnet.Params.Add("x.pe", net.JoinHostPort("127.0.0.1", strconv.Itoa(seeder.LocalPort())))
	state := fixtureState{
		Magnet:   magnet.String(),
		FileName: filepath.Base(source),
		Duration: 120,
		Targets:  []int{40, 80},
	}

	setAppEnvironment(root)
	server, err := api.NewHttpServer(settings.NewSettings())
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	appErrors := make(chan error, 1)
	go func() {
		appErrors <- server.Run(ctx)
	}()
	waitForApp()

	control := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", controlPort),
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/fixture" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(state); err != nil {
				log.Printf("encode fixture state: %v", err)
			}
		}),
	}
	controlErrors := make(chan error, 1)
	go func() {
		controlErrors <- control.ListenAndServe()
	}()

	log.Printf("Full-stack Playwright fixture ready: app=http://127.0.0.1:%d", appPort)
	select {
	case <-ctx.Done():
	case err := <-appErrors:
		if err != nil {
			log.Printf("Cinemator stopped: %v", err)
		}
	case err := <-controlErrors:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("fixture control server stopped: %v", err)
		}
	}

	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := control.Shutdown(shutdownCtx); err != nil {
		log.Printf("stop fixture control server: %v", err)
	}
}

func makeMediaFixture(dir string) string {
	selectedSubtitles := filepath.Join(dir, "selected.srt")
	const selectedSRT = `1
00:00:00,400 --> 00:00:01,800
initial full-stack subtitle

2
00:00:40,200 --> 00:00:42,000
first cold-seek subtitle

3
00:01:20,200 --> 00:01:22,000
second cold-seek subtitle
`
	if err := os.WriteFile(selectedSubtitles, []byte(selectedSRT), 0o644); err != nil {
		log.Fatal(err)
	}

	alternateSubtitles := filepath.Join(dir, "alternate.srt")
	const alternateSRT = `1
00:00:00,400 --> 00:00:01,800
alternate full-stack subtitle
`
	if err := os.WriteFile(alternateSubtitles, []byte(alternateSRT), 0o644); err != nil {
		log.Fatal(err)
	}

	source := filepath.Join(dir, "movie.mkv")
	run("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24000/1001:duration=120",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=120",
		"-i", selectedSubtitles,
		"-i", alternateSubtitles,
		"-map", "0:v:0", "-map", "1:a:0", "-map", "2:s:0", "-map", "3:s:0",
		"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high", "-bf", "2",
		"-g", "48", "-keyint_min", "48", "-sc_threshold", "0",
		"-pix_fmt", "yuv420p", "-b:v", "1200k",
		"-c:a", "eac3", "-b:a", "384k", "-ac", "6",
		"-c:s", "srt",
		"-metadata:s:a:0", "language=rus",
		"-metadata:s:s:0", "language=rus",
		"-metadata:s:s:1", "language=eng",
		source,
	)
	return source
}

func makeMetainfo(source string) (*metainfo.MetaInfo, *metainfo.Info) {
	info := &metainfo.Info{PieceLength: 64 << 10}
	if err := info.BuildFromFilePath(source); err != nil {
		log.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		log.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}
	mi.SetDefaults()
	return mi, info
}

func startSeeder(dataDir string, mi *metainfo.MetaInfo) *torrent.Client {
	config := torrent.NewDefaultClientConfig()
	config.DataDir = dataDir
	config.Seed = true
	config.NoDHT = true
	config.DisableTrackers = true
	config.DisableIPv6 = true
	config.NoDefaultPortForwarding = true
	config.SetListenAddr("127.0.0.1:0")
	config.MaxAllocPeerRequestDataPerConn = 1 << 20
	config.UploadRateLimiter = rate.NewLimiter(512<<10, 1<<20)
	config.KeepAliveTimeout = 30 * time.Second

	client, err := torrent.NewClient(config)
	if err != nil {
		log.Fatal(err)
	}
	seed, _, err := client.AddTorrentSpec(torrent.TorrentSpecFromMetaInfo(mi))
	if err != nil {
		client.Close()
		log.Fatal(err)
	}
	seed.VerifyData()
	deadline := time.Now().Add(20 * time.Second)
	for !seed.Seeding() {
		if time.Now().After(deadline) {
			client.Close()
			log.Fatal("timed out verifying full-stack torrent fixture")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return client
}

func setAppEnvironment(root string) {
	values := map[string]string{
		"CINEMATOR_HLS_PATH":                filepath.Join(root, "hls"),
		"CINEMATOR_DOWNLOAD_PATH":           filepath.Join(root, "download"),
		"CINEMATOR_TOTAL_CACHE_BYTES":       strconv.Itoa(512 << 20),
		"CINEMATOR_MIN_FREE_BYTES":          "0",
		"CINEMATOR_MIN_FREE_INODES":         "0",
		"CINEMATOR_TORRENT_READAHEAD_BYTES": strconv.Itoa(1 << 20),
		"CINEMATOR_HLS_SEGMENT_SECONDS":     "2",
		"CINEMATOR_HLS_WINDOW_SEGMENTS":     "3",
		"CINEMATOR_MAX_TRANSCODES":          "1",
		"CINEMATOR_MAX_PACKAGERS":           "1",
		"CINEMATOR_MAX_QUEUED_JOBS":         "8",
		"CINEMATOR_MAX_JOBS_PER_STREAM":     "4",
		"CINEMATOR_MAX_ACTIVE_STREAMS":      "2",
		"CINEMATOR_HTTP_PORT":               strconv.Itoa(appPort),
		"CINEMATOR_TORRENT_PORT":            "0",
	}
	for key, value := range values {
		if err := os.Setenv(key, value); err != nil {
			log.Fatal(err)
		}
	}
}

func waitForApp() {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/auth/status", appPort)
	deadline := time.Now().Add(30 * time.Second)
	for {
		response, err := http.Get(url)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			log.Fatal("timed out starting Cinemator for full-stack Playwright tests")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func run(name string, args ...string) {
	command := exec.Command(name, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		log.Fatalf("%s failed: %v", name, err)
	}
}
