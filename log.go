package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sync"
	"time"
)

func itoa(buf []byte, i int, wid int) {
	// Assemble decimal in reverse order.
	bp := len(buf) - 1
	for i >= 10 || wid > 1 {
		wid--
		q := i / 10
		buf[bp] = byte('0' + i - q*10)
		bp--
		i = q
	}
	// i < 10
	buf[bp] = byte('0' + i)
}

// Logger allows for log package like logging without the overhead on a simple
// io.Writer
type Logger struct {
	Writer io.Writer
}

// Printf works like log.Printf.
func (l *Logger) Printf(format string, v ...interface{}) {
	in := []byte(fmt.Sprintf(format, v...))
	t := make([]byte, 23+len(in), 23+len(in))

	// Fill in separators
	t[0] = '['
	t[5] = '/'
	t[8] = '/'
	t[11] = ' '
	t[14] = ':'
	t[17] = ':'
	t[20] = ']'
	t[21] = ':'
	t[22] = ' '

	// Fill in values
	n := time.Now()
	year, month, day := n.Date()
	hour, min, sec := n.Clock()
	itoa(t[1:5], int(year), 4)
	itoa(t[6:8], int(month), 2)
	itoa(t[9:11], int(day), 2)
	itoa(t[12:14], int(hour), 2)
	itoa(t[15:17], int(min), 2)
	itoa(t[18:20], int(sec), 2)
	copy(t[23:], in)

	l.Writer.Write(t)
}

// RotateWriter is a io.Writer that can be used for logging with rotation. It
// keeps 1 current logfile and 9 gzipped old logfiles. Rotation occurs when a
// line limit is reached. A RotateWriter *must* be created through
// NewRotateWriter.
type RotateWriter struct {
	fp    *os.File
	wg    sync.WaitGroup
	count int

	// Queue is the internal logging channel. This channel should buffered. If
	// not set, a call to Write will panic.
	Queue chan []byte

	// MaxLines is the maximum amount of lines to have an a file. When a file
	// exceeds this limit, it will be rotated.
	MaxLines int

	// Filename is the name of the first file. Additional files will be named
	// Filename.N.gz, where 0<N<=10.
	Filename string
}

// Shutdown ensures that the file will be closed properly, after having flushed
// all pending lines.
func (w *RotateWriter) Shutdown() {
	close(w.Queue)
	w.wg.Wait()
	w.fp.Close()
}

// Serve executes the run-loop RotateWriter requires to operate.
func (w *RotateWriter) Serve() error {
	if w.Queue == nil {
		return fmt.Errorf("no queue set")
	}
	w.wg.Add(1)
	for entry := range w.Queue {
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
func (w *RotateWriter) Write(output []byte) (int, error) {
	w.Queue <- output
	return len(output), nil
}

// prepare checks if a file can be reused. If not, it will be rotated
// immediately.
func (w *RotateWriter) prepare() error {
	var err error
	if w.fp, err = os.OpenFile(w.Filename, os.O_RDWR|os.O_CREATE, 0666); err != nil {
		w.fp = nil
		return w.rotate()
	}

	// Let's check if the file already present is small enough to continue where
	// we left off.
	var (
		n, i, lc int
		b        = make([]byte, 16*1024)
	)

	// Count the lines.
	for {
		if n, err = w.fp.Read(b); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		for i = 0; i < n; i++ {
			if b[i] == '\n' {
				lc++
			}
		}
	}

	// Reuse?
	if lc >= w.MaxLines {
		w.fp.Close()
		w.fp = nil
		return w.rotate()
	}

	if _, err := w.fp.Write([]byte("\n")); err != nil {
		w.fp.Close()
		w.fp = nil
		return err
	}

	return nil
}

// rotate shuffles the files around and performs GZIP'ing.
func (w *RotateWriter) rotate() error {
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
		p = fmt.Sprintf("%s.%d.gz", w.Filename, i)
		if _, err = os.Stat(p); err != nil {
			// We start in the high end, so we might get a few errors before we reach the first "real" log.
			continue
		}
		os.Rename(p, fmt.Sprintf("%s.%d.gz", w.Filename, i+1))
	}

	// GZIP the newest file.
	b, _ := ioutil.ReadFile(w.Filename)
	buf := new(bytes.Buffer)
	c := gzip.NewWriter(buf)
	c.Write(b)
	c.Close()
	ioutil.WriteFile(fmt.Sprintf("%s.1.gz", w.Filename), buf.Bytes(), 0666)

	// Create a file.
	w.fp, err = os.Create(w.Filename)
	w.count = 0
	return err
}

// NewRotateWriter returns a fully initialized RotateWriter.
func NewRotateWriter(name string, maxlines int) (*RotateWriter, error) {
	w := &RotateWriter{
		Queue:    make(chan []byte, 1024),
		Filename: name,
		MaxLines: maxlines,
	}

	return w, w.prepare()
}
