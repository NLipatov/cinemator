package media

import "strings"

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
