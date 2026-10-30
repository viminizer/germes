package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/viminizer/germes/internal/api"
)

// mcpPrefix names Germes' own tools in claude. The daemon checks them when
// they run, so the hook does not ask twice.
const mcpPrefix = "mcp__germes__"

// CheckHook is `germes hook check`, claude's PreToolUse hook (issue #42): it
// reads the call claude is about to make, asks the daemon (the same block
// list and hooks as the Germes loop) and reports whether it may run. When it
// may not, the reason goes to errOut for claude to read. Anything that keeps
// the daemon from deciding blocks the call.
func CheckHook(ctx context.Context, svc api.Service, sessionID string, in io.Reader, errOut io.Writer) (allowed bool) {
	var call struct {
		Tool  string          `json:"tool_name"`
		Input json.RawMessage `json:"tool_input"`
		Dir   string          `json:"cwd"`
	}
	if err := json.NewDecoder(in).Decode(&call); err != nil || call.Tool == "" {
		fmt.Fprintf(errOut, "Germes could not read this tool call (%v), so it is blocked.\n", err)
		return false
	}
	if strings.HasPrefix(call.Tool, mcpPrefix) {
		return true
	}
	v, err := svc.CheckTool(ctx, api.ToolRequest{SessionID: sessionID, Dir: call.Dir, Name: call.Tool, Args: call.Input})
	if err != nil {
		fmt.Fprintf(errOut, "Germes could not check this tool call (%v), so it is blocked. Try again, or ask the user.\n", err)
		return false
	}
	if v.Reason != "" {
		fmt.Fprintln(errOut, v.Reason)
		return false
	}
	return true
}
