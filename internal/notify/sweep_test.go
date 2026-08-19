package notify

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KamalF/nag/internal/store"
)

// syncBuffer guards the log buffer: the sweep goroutine writes while the
// test polls.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestSweepRunsImmediatelyAtBoot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "nag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateReminder(t.Context(), "due", time.Now().Unix()+1, nil, time.Now().Unix()-10); err != nil {
		t.Fatal(err)
	}

	var logs syncBuffer
	s := NewSweep(st, slog.New(slog.NewTextHandler(&logs, nil)))
	s.interval = time.Hour // only the boot run can fire inside this test
	s.now = func() time.Time { return time.Now().Add(time.Minute) }

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "count=1") {
		select {
		case <-deadline:
			t.Fatalf("boot run did not mark the row; logs: %q", logs.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if strings.Contains(logs.String(), "due") && strings.Contains(logs.String(), "text") {
		t.Errorf("sweep logged reminder text: %q", logs.String())
	}
}
