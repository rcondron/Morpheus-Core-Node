// Package buildinfo carries version metadata stamped in at link time.
package buildinfo

import (
	"fmt"
	"runtime"
)

// Overridden via -ldflags "-X github.com/rcondron/Morpheus-Core-Node/internal/buildinfo.Version=..."
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a one-line human readable version banner.
func String() string {
	return fmt.Sprintf("morcore %s (%s, %s) %s/%s", Version, short(Commit), Date, runtime.GOOS, runtime.GOARCH)
}

// ProtocolVersion is reported to consumers in MOR-RPC pong messages.
func ProtocolVersion() string {
	return "morcore-" + Version
}

func short(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
