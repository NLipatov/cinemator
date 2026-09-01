package ffmpeg

import (
	"cinemator/domain"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidateSelectionRejectsOutOfRangeTracks(t *testing.T) {
	info := domain.MediaInfo{
		AudioTracks: []domain.AudioTrack{{Codec: "aac"}},
		Subtitles:   []domain.SubtitleTrack{{Codec: "subrip"}},
	}
	tests := []struct {
		name      string
		selection StreamSelection
	}{
		{name: "audio", selection: StreamSelection{AudioTrackIndex: 1, SubtitleTrackIndex: -1}},
		{name: "subtitle", selection: StreamSelection{AudioTrackIndex: 0, SubtitleTrackIndex: 1}},
		{name: "negative subtitle", selection: StreamSelection{AudioTrackIndex: 0, SubtitleTrackIndex: -2}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateSelection(info, tt.selection); err == nil {
				t.Fatalf("ValidateSelection(%+v) error = nil, want error", tt.selection)
			}
		})
	}
}

func TestValidateSelectionAllowsDefaultsAndVideoOnlyInput(t *testing.T) {
	tests := []struct {
		name      string
		info      domain.MediaInfo
		selection StreamSelection
	}{
		{name: "default tracks", info: domain.MediaInfo{}, selection: StreamSelection{AudioTrackIndex: 0, SubtitleTrackIndex: -1}},
		{name: "explicit default audio", info: domain.MediaInfo{AudioTracks: []domain.AudioTrack{{}}}, selection: StreamSelection{AudioTrackIndex: -1, SubtitleTrackIndex: -1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateSelection(tt.info, tt.selection); err != nil {
				t.Fatalf("ValidateSelection(%+v) error = %v", tt.selection, err)
			}
		})
	}
}

func TestConverterRejectsInvalidBitmapSubtitleIndexBeforeStartingFFmpeg(t *testing.T) {
	converter := Converter{bitmapSubtitle: -2}
	if err := converter.ConvertToHLS(); err == nil {
		t.Fatal("ConvertToHLS() error = nil, want invalid bitmap subtitle index")
	}
}

func TestRunConversionTasksCancelsAndWaitsForSiblings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	siblingDone := make(chan struct{})
	want := errors.New("video failed")

	err := runConversionTasks(ctx, cancel, []conversionTask{
		{
			name: "video ffmpeg",
			run:  func() error { return want },
		},
		{
			name: "subtitle playlist",
			run: func() error {
				<-ctx.Done()
				close(siblingDone)
				return ctx.Err()
			},
		},
	})

	if !errors.Is(err, want) {
		t.Fatalf("runConversionTasks() error = %v, want %v", err, want)
	}
	if !strings.Contains(err.Error(), "video ffmpeg") {
		t.Fatalf("runConversionTasks() error lacks task name: %v", err)
	}
	select {
	case <-siblingDone:
	default:
		t.Fatal("runConversionTasks() returned before sibling stopped")
	}
}

func TestRunConversionTasksReturnsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runConversionTasks(ctx, cancel, []conversionTask{
		{
			name: "video ffmpeg",
			run: func() error {
				<-ctx.Done()
				return ctx.Err()
			},
		},
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runConversionTasks() error = %v, want %v", err, context.Canceled)
	}
}
