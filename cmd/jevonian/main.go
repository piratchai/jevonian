package main

import (
	"os"

	"github.com/xinyao27/jevonian/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
