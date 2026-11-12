package launch

import (
	"bytes"
)

type AuthStatusBuffer struct {
	bytes.Buffer
	Overflow bool
}

func (b *AuthStatusBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := 65536 - b.Len()
	if len(p) > left {
		b.Overflow = true
		p = p[:left]
	}
	_, err := b.Buffer.Write(p)
	return n, err
}
