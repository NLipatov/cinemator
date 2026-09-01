package ffmpeg

import (
	"cinemator/domain"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// WriteMasterPlaylist links one video rendition to the shared audio and text renditions.
func WriteMasterPlaylist(
	masterPath string,
	videoPlaylist string,
	sharedDir string,
	info domain.MediaInfo,
	selection StreamSelection,
) error {
	if err := ValidateSelection(info, selection); err != nil {
		return err
	}
	masterDir := filepath.Dir(masterPath)
	videoURI, err := filepath.Rel(masterDir, videoPlaylist)
	if err != nil {
		return err
	}
	sharedPrefix, err := filepath.Rel(masterDir, sharedDir)
	if err != nil {
		return err
	}
	if sharedPrefix == "." {
		sharedPrefix = ""
	}
	data := buildMasterPlaylist(
		filepath.ToSlash(videoURI),
		filepath.ToSlash(sharedPrefix),
		info,
		selection,
	)
	return writeFileAtomic(masterPath, []byte(data), 0644)
}

func buildMasterPlaylist(videoURI, sharedPrefix string, info domain.MediaInfo, selection StreamSelection) string {
	selectedAudio := selection.AudioTrackIndex
	if selectedAudio < 0 && len(info.AudioTracks) > 0 {
		selectedAudio = 0
	}
	audioIndices := make([]int, len(info.AudioTracks))
	for i := range audioIndices {
		audioIndices[i] = i
	}
	audioIndices = selectedFirst(audioIndices, selectedAudio)

	selectedTextSubtitle := -1
	if selection.SubtitleTrackIndex >= 0 && !IsBitmapSubtitle(info.Subtitles[selection.SubtitleTrackIndex].Codec) {
		selectedTextSubtitle = selection.SubtitleTrackIndex
	}
	textIndices := textSubtitleIndices(info)
	if selectedTextSubtitle >= 0 {
		textIndices = selectedFirst(textIndices, selectedTextSubtitle)
	}
	withTextSubtitles := !selectedBitmapSubtitle(info, selection) && len(textIndices) > 0

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	for _, i := range audioIndices {
		track := info.AudioTracks[i]
		attrs := []string{
			"TYPE=AUDIO",
			"GROUP-ID=\"audio\"",
			fmt.Sprintf("NAME=\"%s\"", trackName("Audio", i, track.Title)),
			fmt.Sprintf("DEFAULT=%s", yesNo(i == selectedAudio)),
			"AUTOSELECT=YES",
			fmt.Sprintf("URI=\"%s\"", renditionURI(sharedPrefix, fmt.Sprintf("audio_%d.m3u8", i))),
		}
		if language := cleanAttribute(track.Language); language != "" {
			attrs = append(attrs, fmt.Sprintf("LANGUAGE=\"%s\"", language))
		}
		b.WriteString("#EXT-X-MEDIA:" + strings.Join(attrs, ",") + "\n")
	}
	if withTextSubtitles {
		for _, i := range textIndices {
			track := info.Subtitles[i]
			attrs := []string{
				"TYPE=SUBTITLES",
				"GROUP-ID=\"subs\"",
				fmt.Sprintf("NAME=\"%s\"", trackName("Subtitle", i, track.Title)),
				fmt.Sprintf("DEFAULT=%s", yesNo(i == selectedTextSubtitle)),
				fmt.Sprintf("AUTOSELECT=%s", yesNo(selectedTextSubtitle >= 0)),
				"FORCED=NO",
				fmt.Sprintf("URI=\"%s\"", renditionURI(sharedPrefix, fmt.Sprintf("subs_%d.m3u8", i))),
			}
			if language := cleanAttribute(track.Language); language != "" {
				attrs = append(attrs, fmt.Sprintf("LANGUAGE=\"%s\"", language))
			}
			b.WriteString("#EXT-X-MEDIA:" + strings.Join(attrs, ",") + "\n")
		}
	}

	streamAttrs := []string{"BANDWIDTH=2000000"}
	if len(audioIndices) > 0 {
		streamAttrs = append(streamAttrs, "AUDIO=\"audio\"")
	}
	if withTextSubtitles {
		streamAttrs = append(streamAttrs, "SUBTITLES=\"subs\"")
	}
	b.WriteString("#EXT-X-STREAM-INF:" + strings.Join(streamAttrs, ",") + "\n")
	b.WriteString(videoURI + "\n")
	return b.String()
}

func selectedBitmapSubtitle(info domain.MediaInfo, selection StreamSelection) bool {
	return selection.SubtitleTrackIndex >= 0 && IsBitmapSubtitle(info.Subtitles[selection.SubtitleTrackIndex].Codec)
}

func selectedFirst(indices []int, selected int) []int {
	ordered := make([]int, 0, len(indices))
	for _, i := range indices {
		if i == selected {
			ordered = append(ordered, i)
			break
		}
	}
	for _, i := range indices {
		if i != selected {
			ordered = append(ordered, i)
		}
	}
	return ordered
}

func renditionURI(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return strings.TrimSuffix(prefix, "/") + "/" + name
}

func trackName(kind string, index int, title string) string {
	title = cleanAttribute(title)
	if title == "" {
		return fmt.Sprintf("%s %d", kind, index+1)
	}
	return fmt.Sprintf("%s %d — %s", kind, index+1, title)
}

func cleanAttribute(value string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '"' {
			return '\''
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value))
}

func yesNo(value bool) string {
	if value {
		return "YES"
	}
	return "NO"
}
