package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sync"
)

type rotateWriter struct {
	fp    *os.File
	queue chan []byte
	wg    sync.WaitGroup
	count int

	MaxLines int
	Root     string
}

func (w *rotateWriter) Shutdown() {
	close(w.queue)
	w.wg.Wait()
	w.fp.Close()
}

func (w *rotateWriter) Serve() error {
	w.wg.Add(1)
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
	w.wg.Done()

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

	var p string
	for i := 9; i > 0; i-- {
		p = fmt.Sprintf("%s.%d.gz", w.Root, i)
		if _, err = os.Stat(p); err != nil {
			// We start in the high end, so we might get a few errors before we reach the first "real" log.
			continue
		}
		os.Rename(p, fmt.Sprintf("%s.%d.gz", w.Root, i+1))
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

func newRotateWriter(name string, maxlines int) (*rotateWriter, error) {
	r := &rotateWriter{
		queue:    make(chan []byte, 1024),
		Root:     name,
		MaxLines: maxlines,
	}

	var err error
	if r.fp, err = os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0666); err != nil {
		r.fp = nil
		return r, r.rotate()
	}

	// Let's check if the file already present is small enough to continue where
	// we left off.
	var (
		n, i, lc int
		b        = make([]byte, 16*1024)
	)

	for {
		if n, err = r.fp.Read(b); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		for i = 0; i < n; i++ {
			if b[i] == '\n' {
				lc++
			}
		}
	}

	if lc >= maxlines {
		r.fp.Close()
		r.fp = nil
		return r, r.rotate()
	}

	if _, err := r.fp.Write([]byte("\n")); err != nil {
		r.fp.Close()
		r.fp = nil
		return r, err
	}

	return r, nil
}
