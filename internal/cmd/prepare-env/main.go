package main

import (
	"flag"
	"fmt"
	"os"

	"go-harness/internal/config"
)

func main() {
	path := flag.String("config", "", "optional runtime tuning YAML")
	env := flag.String("env-file", "", "deployment profile")
	flag.Parse()
	if err := config.PrepareControllerProfile(*path, *env); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
