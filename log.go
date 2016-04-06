package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io/ioutil"
	"os"
	"sync"
	"time"
)

type RotateWriter struct {
	lock sync.Mutex
	fp   *os.File
	last time.Time

	Interval time.Duration
	Root     string
}

// Write satisfies the io.Writer interface.
func (w *RotateWriter) Write(output []byte) (int, error) {
	n := time.Now()
	if n.After(w.last.Add(w.Interval)) || w.fp == nil {
		err := w.Rotate()
		if err != nil {
			w.fp = nil
		}
	}

	w.lock.Lock()
	defer w.lock.Unlock()
	return w.fp.Write(output)
}

// Perform the actual act of rotating and reopening file.
func (w *RotateWriter) Rotate() error {
	w.lock.Lock()
	defer w.lock.Unlock()

	w.last = time.Now()

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
	return err
}
