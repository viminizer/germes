// Package mcp is `germes mcp`: a stdio MCP server the claude CLI starts for
// each turn. It lists the session's Germes tools and forwards each call to
// the daemon, which runs it like the Germes loop does (block list, limits).
// It imports only api.
package mcp

import (
	"context"
	"encoding/json"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/viminizer/germes/internal/api"
)

// Run serves the session's tools on stdin and stdout until the client
// closes them.
func Run(ctx context.Context, svc api.Service, sessionID, version string) error {
	tools, err := svc.ListTools(ctx, sessionID)
	if err != nil {
		return err
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "germes", Version: version}, nil)
	for _, t := range tools {
		server.AddTool(&sdk.Tool{Name: t.Name, Description: t.Description, InputSchema: t.Schema},
			func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				args := req.Params.Arguments
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				res, err := svc.RunTool(ctx, api.ToolRequest{SessionID: sessionID, Name: t.Name, Args: args})
				if err != nil {
					res = api.ToolResult{Output: err.Error(), IsError: true}
				}
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: res.Output}}, IsError: res.IsError}, nil
			})
	}
	return server.Run(ctx, &sdk.StdioTransport{})
}
