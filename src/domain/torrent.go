package domain

type FileInfo struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
}
