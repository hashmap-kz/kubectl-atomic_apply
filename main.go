package main

import (
	"fmt"
	"os"

	"github.com/hashmap-kz/katomik/cmd"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

func main() {
	streams := genericiooptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
	rootCmd := cmd.NewRootCmd(streams)
	if err := rootCmd.Execute(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error executing cmd: %v\n", err)
		os.Exit(1)
	}
}
