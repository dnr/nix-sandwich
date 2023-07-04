package main

import (
	"fmt"
	"io"

	"golang.org/x/exp/constraints"
)

func min[T constraints.Ordered](a, b T) T {
	if a < b {
		return a
	}
	return b
}

func panicIfErr(err error) {
	if err != nil {
		panic(err)
	}
}

func panicIfTrue(b bool, msg string) {
	if b {
		panic(msg)
	}
}

func panicIfErr2[T any](_ T, err error) {
	if err != nil {
		panic(err)
	}
}

func ioCopy(dst io.Writer, src io.Reader, buf []byte, expected int64) error {
	if buf == nil {
		buf = make([]byte, 128*1024)
	}
	if n, err := io.CopyBuffer(dst, src, buf); err != nil {
		return err
	} else if expected >= 0 && n != expected {
		return fmt.Errorf("expected %d bytes, got %d", expected, n)
	}
	return nil
}
