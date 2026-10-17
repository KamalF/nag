package main

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

// §10.1's order: the server stops, the sweep is canceled and waited for,
// and only then is the database closed — never under a tick still writing.
func TestServeUntilShutdownOrder(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(e string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}

	stop, cancel := context.WithCancel(context.Background())
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler()}
	server.RegisterOnShutdown(func() { record("server shutdown") })
	sweep := func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // a tick finishing its last write
		record("sweep returned")
	}
	closeDB := func() error {
		record("db closed")
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- serveUntil(stop, server, sweep, closeDB) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntil did not return after the stop signal")
	}

	want := []string{"server shutdown", "sweep returned", "db closed"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// A server that cannot listen still stops the sweep and closes the
// database before reporting the error.
func TestServeUntilListenFailure(t *testing.T) {
	closed := false
	err := serveUntil(context.Background(), &http.Server{Addr: "not-an-address"},
		func(ctx context.Context) { <-ctx.Done() },
		func() error { closed = true; return nil })
	if err == nil || !closed {
		t.Errorf("err = %v, db closed = %v", err, closed)
	}
}
