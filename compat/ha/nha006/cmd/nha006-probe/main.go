package main

import (
	"fmt"
	"io"
	"os"

	"github.com/damione1/Homeward/compat/ha/nha006"
)

func main() {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail(err)
	}
	output, err := nha006.RunJSON(input)
	if err != nil {
		fail(err)
	}
	if _, err := os.Stdout.Write(append(output, '\n')); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
