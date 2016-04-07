package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io/ioutil"
	"os"
)

type rotateWriter struct {
	fp    *os.File
	queue chan []byte
	count int

	MaxLines int
	Root     string
}

func (w *rotateWriter) Serve() error {
	for entry := range w.queue {
		if w.fp == nil || w.count > w.MaxLines {
			err := w.rotate()
			if err != nil {
				return err
			}
		}

		w.count++
		_, err := w.fp.Write(entry)
		if err != nil {
			return err
		}
	}

	return nil
}

// Write satisfies the io.Writer interface.
func (w *rotateWriter) Write(output []byte) (int, error) {
	w.queue <- output
	return len(output), nil
}

func (w *rotateWriter) rotate() error {
	var err error

	// Close existing file if open
	if w.fp != nil {
		err = w.fp.Close()
		w.fp = nil
		if err != nil {
			return err
		}
	}

	for i := 9; i > 0; i-- {
		os.Rename(fmt.Sprintf("%s.%d.gz", w.Root, i), fmt.Sprintf("%s.%d.gz", w.Root, i+1))
	}

	b, _ := ioutil.ReadFile(w.Root)
	buf := new(bytes.Buffer)
	c := gzip.NewWriter(buf)
	c.Write(b)
	c.Close()
	ioutil.WriteFile(fmt.Sprintf("%s.1.gz", w.Root), buf.Bytes(), 0666)

	// Create a file.
	w.fp, err = os.Create(w.Root)
	w.count = 0
	return err
}

func newRotateWriter(name string, maxlines int) *rotateWriter {
	return &rotateWriter{
		queue:    make(chan []byte, 1024),
		Root:     name,
		MaxLines: maxlines,
	}
}
