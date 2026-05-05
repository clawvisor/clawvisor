package scenario

import (
	"os"
	"time"
)

func writeFileImpl(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func nowPtr() *time.Time {
	t := time.Now().UTC()
	return &t
}
