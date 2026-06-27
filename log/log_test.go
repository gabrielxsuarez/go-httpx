package log

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeliversAllLinesAndAttrs(t *testing.T) {
	var buf bytes.Buffer
	logs := New(func(string) io.Writer { return &buf }, WithApp("test"), WithFlushInterval(10*time.Millisecond))

	for i := 0; i < 100; i++ {
		logs.Access().Info("hit", "n", i)
	}
	if err := logs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := nonEmptyLines(buf.String())
	if len(lines) != 100 {
		t.Fatalf("got %d lines, want 100", len(lines))
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if first["app"] != "test" || first["log"] != NameAccess || first["msg"] != "hit" {
		t.Fatalf("missing/wrong attrs: %v", first)
	}
}

func TestConcurrentWritersLoseNothingWhenBlocking(t *testing.T) {
	var buf syncBuffer
	logs := New(func(string) io.Writer { return &buf },
		WithBlockOnFull(true), WithQueueSize(8), WithFlushInterval(5*time.Millisecond))

	const goroutines, perG = 20, 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lg := logs.Event()
			for i := 0; i < perG; i++ {
				lg.Info("e")
			}
		}()
	}
	wg.Wait()
	logs.Close()

	if got := len(nonEmptyLines(buf.String())); got != goroutines*perG {
		t.Fatalf("got %d lines, want %d", got, goroutines*perG)
	}
	if d := logs.Dropped(); d != 0 {
		t.Fatalf("dropped %d lines while blocking", d)
	}
}

func TestNilSinkDiscards(t *testing.T) {
	logs := New(func(string) io.Writer { return nil })
	logs.Warning().Warn("ignored") // must not panic
	if err := logs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLevelsAndCustomName(t *testing.T) {
	var buf bytes.Buffer
	logs := New(func(string) io.Writer { return &buf }, WithFlushInterval(5*time.Millisecond))
	logs.Named("audit").Info("checked")
	logs.Close()

	var rec map[string]any
	if err := json.Unmarshal([]byte(nonEmptyLines(buf.String())[0]), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec["log"] != "audit" {
		t.Fatalf("log attr = %v, want audit", rec["log"])
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// syncBuffer is a bytes.Buffer safe for the single writer goroutine plus the
// test reader; the asyncWriter already serializes writes, but Close happens
// before the read so a plain lock keeps -race quiet.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
