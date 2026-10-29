// Package stream sends on channels and waits for goroutines without blocking
// forever.
package stream

import (
	"context"
	"sync"
)

// Send puts v on ch, or gives up when ctx ends. It reports whether v was sent.
func Send[T any](ctx context.Context, ch chan<- T, v T) bool {
	select {
	case ch <- v:
		return true
	case <-ctx.Done():
		return false
	}
}

// Wait waits until wg is done, or gives up with ctx's error when ctx ends.
func Wait(ctx context.Context, wg *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
