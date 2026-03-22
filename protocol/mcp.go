// Package protocol implements the subset of MCP JSON-RPC needed for intermcp.
package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// JSON-RPC types.

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Server reads JSON-RPC requests from stdin and writes responses to stdout.
type Server struct {
	handlers map[string]HandlerFunc
	mu       sync.Mutex
	writer   io.Writer
}

type HandlerFunc func(ctx context.Context, params json.RawMessage) (any, error)

func NewServer(w io.Writer) *Server {
	return &Server{
		handlers: make(map[string]HandlerFunc),
		writer:   w,
	}
}

func (s *Server) Handle(method string, fn HandlerFunc) {
	s.handlers[method] = fn
}

// Notify sends a JSON-RPC notification (no id, no response expected).
func (s *Server) Notify(method string, params any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := Notification{JSONRPC: "2.0", Method: method, Params: params}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.writer, "%s\n", data)
	return err
}

// Run reads newline-delimited JSON-RPC from r until EOF or ctx cancellation.
func (s *Server) Run(ctx context.Context, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		// Notifications have no id — ignore them.
		if req.ID == nil {
			continue
		}
		handler, ok := s.handlers[req.Method]
		if !ok {
			s.respond(req.ID, nil, &RPCError{Code: -32601, Message: "method not found: " + req.Method})
			continue
		}
		result, err := handler(ctx, req.Params)
		if err != nil {
			s.respond(req.ID, nil, &RPCError{Code: -32000, Message: err.Error()})
			continue
		}
		s.respond(req.ID, result, nil)
	}
	return scanner.Err()
}

func (s *Server) respond(id json.RawMessage, result any, rpcErr *RPCError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := Response{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr}
	data, err := json.Marshal(resp)
	if err != nil {
		// Fall back to an error response if the result can't be marshaled.
		resp = Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: -32603, Message: "internal: " + err.Error()}}
		data, _ = json.Marshal(resp)
	}
	fmt.Fprintf(s.writer, "%s\n", data)
}
