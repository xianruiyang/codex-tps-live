package main

import (
	codextps "github.com/adenta/codex-tps"
	"os"
)

func main() { os.Exit(codextps.RunLive(os.Args[1:], os.Stdout, os.Stderr)) }
