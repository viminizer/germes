package shell

import (
	"context"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/env"
	"github.com/viminizer/germes/internal/tool"
)

func TestFailingCommandIsAnError(t *testing.T) {
	bash := New(env.NewLocal())[0]
	_, err := bash.Run(context.Background(), tool.Call{Dir: t.TempDir(), Args: []byte(`{"command":"echo half done; exit 3"}`)})
	if err == nil || !strings.Contains(err.Error(), "half done") || !strings.Contains(err.Error(), "[exit code 3]") {
		t.Fatalf("got %v, want an error with the output and the exit code", err)
	}
}
