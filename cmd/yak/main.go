package main

import (
	"context"
	"fmt"
	"os"

	"github.com/blackstorm/bsdyak/internal/cli"
)

func main() {
	root := cli.NewRootCmd()
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "yak: %v\n", err)
		os.Exit(1)
	}
}
