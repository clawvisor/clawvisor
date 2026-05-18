package main

import (
	"os"

	"github.com/clawvisor/clawvisor/internal/clawvisorcli"
)

func main() {
	if err := clawvisorcli.Execute(); err != nil {
		os.Exit(1)
	}
}
