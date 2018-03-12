package main

import (
	"io"
	"sync"
)

type triggerWriter struct {
	once sync.Once
	ch   chan struct{}

	w io.WriteCloser
}

func newTriggerWriter(w io.WriteCloser) *triggerWriter {
	return &triggerWriter{
		ch: make(chan struct{}),
		w:  w,
	}
}

func (w *triggerWriter) Trigger() <-chan struct{} {
	return w.ch
}

func (w *triggerWriter) trigger() {
	close(w.ch)
}

func (w *triggerWriter) Write(b []byte) (n int, err error) {
	w.once.Do(w.trigger)

	return w.w.Write(b)
}

func (w *triggerWriter) Close() error {
	w.once.Do(w.trigger)

	return w.w.Close()
}
