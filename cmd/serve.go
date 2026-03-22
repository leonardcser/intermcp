package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/leoadberg/intermcp/daemon"
	"github.com/leoadberg/intermcp/protocol"
)

// daemonClient wraps a connection to the daemon with a single reader goroutine
// that dispatches responses to request/response calls and pushes messages via channel.
type daemonClient struct {
	conn    net.Conn
	writer  sync.Mutex
	pending chan daemon.Envelope // buffered channel for request/response pairs
	pushes  chan daemon.Envelope // pushed messages (type "message")
}

func newDaemonClient(conn net.Conn) *daemonClient {
	dc := &daemonClient{
		conn:    conn,
		pending: make(chan daemon.Envelope, 64),
		pushes:  make(chan daemon.Envelope, 256),
	}
	go dc.readLoop()
	return dc
}

func (dc *daemonClient) readLoop() {
	scanner := bufio.NewScanner(dc.conn)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		var env daemon.Envelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		if env.Type == daemon.TypeMessage {
			dc.pushes <- env
		} else {
			dc.pending <- env
		}
	}
	close(dc.pushes)
	close(dc.pending)
}

// request sends an envelope and waits for the next response.
func (dc *daemonClient) request(env daemon.Envelope) (daemon.Envelope, error) {
	dc.writer.Lock()
	data, _ := json.Marshal(env)
	data = append(data, '\n')
	_, err := dc.conn.Write(data)
	dc.writer.Unlock()
	if err != nil {
		return daemon.Envelope{}, err
	}
	resp, ok := <-dc.pending
	if !ok {
		return daemon.Envelope{}, fmt.Errorf("connection closed")
	}
	return resp, nil
}

func Serve() {
	if err := daemon.EnsureRunning(); err != nil {
		log.Fatalf("failed to start daemon: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := net.Dial("tcp", daemon.Addr)
	if err != nil {
		log.Fatalf("failed to connect to daemon: %v", err)
	}
	defer conn.Close()

	dc := newDaemonClient(conn)
	parentPID := os.Getppid()
	if override := os.Getenv("INTERMCP_PID"); override != "" {
		fmt.Sscanf(override, "%d", &parentPID)
	}

	project := projectRoot()

	// Register with the daemon and consume the ack.
	if _, err := dc.request(daemon.Envelope{Type: daemon.TypeRegister, From: parentPID, Project: project}); err != nil {
		log.Fatalf("failed to register with daemon: %v", err)
	}

	// Set up the MCP server over stdio.
	mcpServer := protocol.NewServer(os.Stdout)
	setupMCPHandlers(mcpServer, dc, parentPID)

	// Forward pushed messages as channel notifications.
	go func() {
		for env := range dc.pushes {
			select {
			case <-ctx.Done():
				return
			default:
			}
			var body struct {
				Message string `json:"message"`
			}
			json.Unmarshal(env.Body, &body)
			mcpServer.Notify("notifications/claude/channel", map[string]any{
				"content": body.Message,
				"meta": map[string]string{
					"from_pid": fmt.Sprintf("%d", env.From),
				},
			})
		}
	}()

	if err := mcpServer.Run(ctx, os.Stdin); err != nil {
		log.Fatalf("mcp server: %v", err)
	}
}

func setupMCPHandlers(s *protocol.Server, dc *daemonClient, pid int) {
	s.Handle("initialize", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{
			"protocolVersion": "2025-03-26",
			"serverInfo":      map[string]string{"name": "intermcp", "version": "0.1.0"},
			"capabilities": map[string]any{
				"experimental": map[string]any{"claude/channel": map[string]any{}},
				"tools":        map[string]any{},
			},
			"instructions": "You are connected to intermcp, an inter-agent communication channel.\n" +
				"Messages from other Claude Code agents arrive as <channel source=\"intermcp\" from_pid=\"...\">.\n" +
				"Use the `list_agents` tool to discover other running agents.\n" +
				"Use the `send` tool to send a message to one or more agents by PID.\n" +
				"When you receive a channel message, respond helpfully — you are collaborating with the sender.",
		}, nil
	})

	s.Handle("notifications/initialized", func(_ context.Context, _ json.RawMessage) (any, error) {
		return nil, nil
	})

	s.Handle("tools/list", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{
			"tools": []map[string]any{
				{
					"name":        "list_agents",
					"description": "List running Claude Code agents connected to intermcp. By default only shows agents in the same project (git repo).",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"all": map[string]any{
								"type":        "boolean",
								"description": "If true, list agents across all projects.",
							},
						},
					},
				},
				{
					"name":        "send",
					"description": "Send a message to one or more Claude Code agents by PID. The message is delivered instantly via their channel.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"to": map[string]any{
								"type": "array",
								"items": map[string]any{
									"type": "integer",
								},
								"description": "The PIDs of the target agents.",
							},
							"message": map[string]any{
								"type":        "string",
								"description": "The message to send.",
							},
						},
						"required": []string{"to", "message"},
					},
				},
			},
		}, nil
	})

	// Mutex to serialize request/response pairs to the daemon.
	var reqMu sync.Mutex

	s.Handle("tools/call", func(_ context.Context, params json.RawMessage) (any, error) {
		var req struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, fmt.Errorf("bad request: %w", err)
		}

		switch req.Name {
		case "list_agents":
			var args struct {
				All bool `json:"all"`
			}
			json.Unmarshal(req.Arguments, &args)
			reqMu.Lock()
			resp, err := dc.request(daemon.Envelope{Type: daemon.TypeList, All: args.All})
			reqMu.Unlock()
			if err != nil {
				return toolError("failed to list agents: " + err.Error()), nil
			}
			var agents []daemon.Agent
			json.Unmarshal(resp.Body, &agents)
			lines := fmt.Sprintf("%d connected agent(s):\n", len(agents))
			for _, a := range agents {
				name := a.Name
				if name == "" {
					name = "(unnamed)"
				}
				marker := ""
				if a.PID == pid {
					marker = " (you)"
				}
				if args.All && a.Project != "" {
					lines += fmt.Sprintf("  PID %d — %s [%s]%s\n", a.PID, name, a.Project, marker)
				} else {
					lines += fmt.Sprintf("  PID %d — %s%s\n", a.PID, name, marker)
				}
			}
			return toolResult(lines), nil

		case "send":
			var args struct {
				To      []int  `json:"to"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(req.Arguments, &args); err != nil {
				return toolError("bad arguments: " + err.Error()), nil
			}
			if len(args.To) == 0 {
				return toolError("at least one recipient PID is required"), nil
			}
			body, _ := json.Marshal(map[string]string{"message": args.Message})
			var errors []string
			var sent []int
			for _, to := range args.To {
				reqMu.Lock()
				resp, err := dc.request(daemon.Envelope{
					Type: daemon.TypeSend,
					From: pid,
					To:   to,
					Body: body,
				})
				reqMu.Unlock()
				if err != nil {
					errors = append(errors, fmt.Sprintf("PID %d: %s", to, err.Error()))
				} else if resp.Type == daemon.TypeError {
					errors = append(errors, fmt.Sprintf("PID %d: %s", to, daemonError(resp.Body)))
				} else {
					sent = append(sent, to)
				}
			}
			var result string
			if len(sent) > 0 {
				result = fmt.Sprintf("Message sent to %d agent(s).", len(sent))
			}
			if len(errors) > 0 {
				result += "\nErrors:\n" + strings.Join(errors, "\n")
			}
			if len(sent) == 0 {
				return toolError(result), nil
			}
			return toolResult(result), nil

		default:
			return nil, fmt.Errorf("unknown tool: %s", req.Name)
		}
	})
}

// projectRoot returns the root of the git repository for the current working
// directory, falling back to the working directory itself.
func projectRoot() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

func daemonError(body json.RawMessage) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
		return parsed.Error
	}
	return string(body)
}

func toolResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
}

func toolError(text string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
}
