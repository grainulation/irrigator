package messaging

import (
	"github.com/grainulation/irrigator/internal/review"
)

func ReviewReadFile(path string, limit int64) ([]byte, error) { return review.ReadFile(path, limit) }
