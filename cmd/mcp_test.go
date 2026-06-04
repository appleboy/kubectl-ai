// Copyright 2025 Google LLC
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

package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/kubectl-ai/gollm"
	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/mcp"
	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/mcpauth"
	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/tools"
)

func TestKubectlMCPServerHTTPClientIntegration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	toolset := tools.Tools{}
	toolset.Init()
	toolset.RegisterTool(&stubTool{})

	port := getFreePort(t)

	workDir := t.TempDir()

	server, err := newKubectlMCPServer(ctx, "", toolset, workDir, false, "streamable-http", port, mcpauth.Config{})
	if err != nil {
		t.Fatalf("failed to create MCP server: %v", err)
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Serve(ctx)
	}()

	waitForHTTPServer(t, port)

	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server exited early: %v", err)
		}
	default:
	}

	clientConfig := mcp.ClientConfig{
		Name:         "test-client",
		URL:          fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
		UseStreaming: true,
		Timeout:      5,
	}

	client := mcp.NewClient(clientConfig)

	connectCtx, connectCancel := context.WithTimeout(ctx, 5*time.Second)
	defer connectCancel()

	t.Log("connecting client")

	if err := client.Connect(connectCtx); err != nil {
		t.Fatalf("failed to connect client to MCP server: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close MCP client: %v", err)
		}
	}()

	toolsCtx, toolsCancel := context.WithTimeout(ctx, 5*time.Second)
	defer toolsCancel()

	t.Log("listing tools")

	availableTools, err := client.ListTools(toolsCtx)
	if err != nil {
		t.Fatalf("failed to list tools from MCP server: %v", err)
	}

	t.Logf("retrieved %d tool(s)", len(availableTools))

	if !toolExists("stub", availableTools) {
		t.Fatalf("expected to find stub tool, got %v", availableTools)
	}

	cancel()

	select {
	case <-serverErr:
	case <-time.After(500 * time.Millisecond):
	}
}

func waitForHTTPServer(t *testing.T, port int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	address := fmt.Sprintf("127.0.0.1:%d", port)

	for {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start listening on %s: %v", address, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func getFreePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to acquire free port: %v", err)
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port
}

func toolExists(name string, tools []mcp.Tool) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

type stubTool struct{}

func (stubTool) Name() string {
	return "stub"
}

func (stubTool) Description() string {
	return "stub tool"
}

func (stubTool) FunctionDefinition() *gollm.FunctionDefinition {
	return &gollm.FunctionDefinition{
		Name:        "stub",
		Description: "stub tool",
		Parameters: &gollm.Schema{
			Type: gollm.TypeObject,
		},
	}
}

func (stubTool) Run(context.Context, map[string]any) (any, error) {
	return "ok", nil
}

func (stubTool) IsInteractive(map[string]any) (bool, error) {
	return false, nil
}

func (stubTool) CheckModifiesResource(map[string]any) string {
	return "no"
}
