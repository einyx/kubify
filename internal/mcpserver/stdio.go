package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// ServeStdio runs the MCP server over stdin/stdout (newline-delimited
// JSON-RPC 2.0). This is the local-agent transport: register with
//
//	claude mcp add kubo -- kubo-mcp
//
// and Claude Code (or any MCP client) gets the Stack Platform tools backed
// by the caller's kubeconfig. Stdio is inherently local — no auth needed.
func ServeStdio(ctx context.Context, s *Server, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	outW := bufio.NewWriter(out)
	for {
		select {
		case <-ctx.Done():
			return outW.Flush()
		default:
		}
		if !scanner.Scan() {
			return outW.Flush()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			resp := errResp(nil, -32700, "parse error: "+err.Error())
			if b, err := json.Marshal(resp); err == nil {
				fmt.Fprintln(outW, string(b))
				outW.Flush()
			}
			continue
		}
		resp := s.dispatch(ctx, &req)
		b, err := json.Marshal(resp)
		if err != nil {
			return fmt.Errorf("marshal response: %w", err)
		}
		fmt.Fprintln(outW, string(b))
		outW.Flush()
	}
}

// ServeStdioDefault runs ServeStdio against the process stdin/stdout.
func ServeStdioDefault(ctx context.Context, s *Server) error {
	return ServeStdio(ctx, s, os.Stdin, os.Stdout)
}
