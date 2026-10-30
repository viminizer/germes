package loop

import (
	"context"

	"github.com/viminizer/germes/internal/prompt"
)

// CreateTitle names a session with the engine's model (engine.Titler).
func (l *Loop) CreateTitle(ctx context.Context, message string) (string, error) {
	return l.ask(ctx, prompt.Title, message)
}
