package apikeys

import (
	"os"
	"time"
)

func osChtimes(path string, t time.Time) error { return os.Chtimes(path, t, t) }
