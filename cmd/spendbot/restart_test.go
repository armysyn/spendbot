package main

import (
	"sync"
	"testing"
	"time"
)

// After an update the new program starts even if some background work does not stop.
func TestWaitTimeout(t *testing.T) {
	var wg sync.WaitGroup
	if !waitTimeout(&wg, time.Second) {
		t.Fatal("nothing running: done at once")
	}
	wg.Add(1)
	start := time.Now()
	if waitTimeout(&wg, 50*time.Millisecond) || time.Since(start) > time.Second {
		t.Fatal("stuck work must not hold the restart")
	}
	go func() { time.Sleep(20 * time.Millisecond); wg.Done() }()
	if !waitTimeout(&wg, time.Second) {
		t.Fatal("work that stops is waited for")
	}
}
