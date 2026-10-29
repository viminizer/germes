// Package stream sends on channels without blocking forever.
package stream

import "context"

// Send puts v on ch, or gives up when ctx ends. It reports whether v was sent.
func Send[T any](ctx context.Context, ch chan<- T, v T) bool {
	select {
	case ch <- v:
		return true
	case <-ctx.Done():
		return false
	}
}
