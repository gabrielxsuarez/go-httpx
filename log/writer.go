package log

import (
	"bufio"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// asyncWriter decouples logging from disk. The caller's goroutine only copies
// the formatted line and hands it to a buffered channel; a single background
// goroutine drains the channel into a bufio.Writer and flushes it on an
// interval. Because that goroutine is the only one touching the bufio.Writer,
// no per-write mutex is needed, and the write syscall never happens on the hot
// path (e.g. the request goroutine).
//
// The trade-off is durability: lines sit in the channel and the bufio buffer
// until flushed, so a hard crash can lose the most recent ones. Close drains
// and flushes for the graceful path.
type asyncWriter struct {
	ch        chan []byte
	done      chan struct{}
	bw        *bufio.Writer
	block     bool
	dropped   atomic.Uint64
	closeOnce sync.Once
}

func newAsyncWriter(sink io.Writer, queue, bufSize int, flush time.Duration, block bool) *asyncWriter {
	if flush <= 0 {
		flush = time.Second
	}
	a := &asyncWriter{
		ch:    make(chan []byte, queue),
		done:  make(chan struct{}),
		bw:    bufio.NewWriterSize(sink, bufSize),
		block: block,
	}
	go a.loop(flush)
	return a
}

// Write copies p and queues it for the writer goroutine. slog reuses the buffer
// it passes to Write once Handle returns, so the bytes must be copied before
// they cross the channel. With block off (the default), a full queue drops the
// line and bumps a counter instead of stalling the caller; with block on, the
// caller waits for room, trading latency for not losing lines.
func (a *asyncWriter) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)

	if a.block {
		a.ch <- b
		return len(p), nil
	}
	select {
	case a.ch <- b:
	default:
		a.dropped.Add(1)
	}
	return len(p), nil
}

func (a *asyncWriter) loop(flush time.Duration) {
	t := time.NewTicker(flush)
	defer t.Stop()
	for {
		select {
		case b, ok := <-a.ch:
			if !ok {
				a.bw.Flush()
				close(a.done)
				return
			}
			a.bw.Write(b)
		case <-t.C:
			a.bw.Flush()
		}
	}
}

// Close stops accepting new lines, drains the ones already queued, flushes the
// buffer and returns. It is safe to call more than once. Callers must ensure no
// Write happens after Close (e.g. close it after the HTTP server has stopped
// serving), since a send on the closed channel would panic.
func (a *asyncWriter) Close() error {
	a.closeOnce.Do(func() { close(a.ch) })
	<-a.done
	return nil
}
