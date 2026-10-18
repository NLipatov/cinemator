package media

import "strings"

func PlaylistDuration(data []byte) (float64, error) {
	segments, _, err := parseMediaPlaylist(data)
	if err != nil {
		return 0, err
	}
	return playlistDuration(segments), nil
}

func HasSegment(data string) bool {
	expectURI := false
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF") {
			expectURI = true
			continue
		}
		if expectURI && !strings.HasPrefix(line, "#") {
			return true
		}
	}
	return false
}
