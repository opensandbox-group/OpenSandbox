// Copyright 2025 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command mcpecho is a minimal stdio MCP server used by execd tests and
// smoke tests. It exposes one tool ("echo") that echoes its message input
// and one prompt/resource surface for bridging coverage.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoIn struct {
	Message string `json:"message" jsonschema:"the message to echo"`
}

type echoOut struct {
	Echo   string `json:"echo" jsonschema:"the echoed message"`
	PidEnv string `json:"pid_env" jsonschema:"value of MCPECHO_MARKER from the environment"`
}

func main() {
	serve := flag.String("serve", "stdio", "transport to serve: stdio|http")
	addr := flag.String("addr", "127.0.0.1:0", "listen address for -serve=http")
	flag.Parse()

	server := mcp.NewServer(&mcp.Implementation{Name: "mcpecho", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo the message back"}, func(ctx context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, echoOut, error) {
		out := echoOut{Echo: in.Message, PidEnv: os.Getenv("MCPECHO_MARKER")}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Message}}}, out, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "explode", Description: "Always fails"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, fmt.Errorf("intentional failure")
	})

	switch *serve {
	case "http":
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
		if err := (&http.Server{Addr: *addr, Handler: handler}).ListenAndServe(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
