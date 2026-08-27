package main

import (
	"os"

	codextps "github.com/adenta/codex-tps"
)

func main() { os.Exit(codextps.Run(os.Args[1:], os.Stdout, os.Stderr)) }
