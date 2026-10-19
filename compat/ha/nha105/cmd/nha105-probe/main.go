package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/damione1/Homeward/compat/ha/nha105"
)

func main() {
	input, err := io.ReadAll(io.LimitReader(os.Stdin, (64<<10)+1))
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := nha105.RunJSON(ctx, input)
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
