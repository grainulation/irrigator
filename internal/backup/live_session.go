package backup

import (
	"regexp"
)

var RegistryRunID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
