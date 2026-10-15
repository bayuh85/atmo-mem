package main

import (
	"os"

	"github.com/bayuh85/atmo-mem/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
