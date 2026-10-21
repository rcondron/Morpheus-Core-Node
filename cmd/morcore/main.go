// Command morcore is the Morpheus Core Node: a lightweight, terminal-first
// compute routing node for the Morpheus decentralized AI marketplace.
package main

import (
	"os"

	"github.com/rcondron/Morpheus-Core-Node/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
