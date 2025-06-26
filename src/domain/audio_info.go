package domain

type AudioInfo struct {
	Index          int
	Codec          string
	Tag            AudioTag
	NeedsTranscode bool // true if not AAC
}

type AudioTag struct {
	Title, Language string
}
