package domain

import "time"

type DirInfo struct {
	Hash     string
	Name     string
	SizeGB   float64
	ExpireAt time.Time
}
