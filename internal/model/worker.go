package model

import (
	"time"
)

type Options struct {
	Workers int
	Timeout time.Duration
	Budget  float64
	Retry   bool
}
