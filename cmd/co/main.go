package main

import (
	"fmt"
	"os"

	"git.co.codes/co/co/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "co:", err)
		os.Exit(1)
	}
}
