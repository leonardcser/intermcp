// Package daemon implements the shared TCP relay that routes messages between agents.
package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const Addr = "127.0.0.1:7779"

// Envelope types.
const (
	TypeRegister   = "register"
	TypeRegistered = "registered"
	TypeList       = "list"
	TypeAgents     = "agents"
	TypeSend       = "send"
	TypeSent       = "sent"
	TypeMessage    = "message"
	TypeError      = "error"
)

// Envelope is the wire format between MCP serve instances and the daemon.
type Envelope struct {
	Type    string          `json:"type"`
	From    int             `json:"from,omitempty"`
	To      int             `json:"to,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
	Name    string          `json:"name,omitempty"`
	Project string          `json:"project,omitempty"`
	All     bool            `json:"all,omitempty"`
}

// Agent tracks a connected MCP serve instance.
type Agent struct {
	PID     int    `json:"pid"`
	Name    string `json:"name,omitempty"`
	Project string `json:"project,omitempty"`
	conn    net.Conn
}

// Daemon is the central relay.
type Daemon struct {
	mu     sync.RWMutex
	agents map[int]*Agent // keyed by PID
}

func New() *Daemon {
	return &Daemon{agents: make(map[int]*Agent)}
}

func (d *Daemon) ListenAndServe() error {
	ln, err := net.Listen("tcp", Addr)
	if err != nil {
		return err
	}
	log.Printf("intermcp daemon listening on %s", Addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go d.handleConn(conn)
	}
}

func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	var agentPID int
	defer func() {
		if agentPID != 0 {
			d.mu.Lock()
			delete(d.agents, agentPID)
			d.mu.Unlock()
			log.Printf("agent %d disconnected", agentPID)
		}
	}()

	for scanner.Scan() {
		var env Envelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		switch env.Type {
		case TypeRegister:
			agentPID = env.From
			d.mu.Lock()
			d.agents[agentPID] = &Agent{PID: agentPID, Name: env.Name, Project: env.Project, conn: conn}
			d.mu.Unlock()
			log.Printf("agent %d registered (name=%q, project=%q)", agentPID, env.Name, env.Project)
			d.sendTo(conn, Envelope{Type: TypeRegistered})

		case TypeList:
			d.mu.RLock()
			sender := d.agents[agentPID]
			agents := make([]Agent, 0, len(d.agents))
			for _, a := range d.agents {
				if !d.sameProject(sender, a, env.All) {
					continue
				}
				agents = append(agents, Agent{PID: a.PID, Name: a.Name, Project: a.Project})
			}
			d.mu.RUnlock()
			body, _ := json.Marshal(agents)
			d.sendTo(conn, Envelope{Type: TypeAgents, Body: body})

		case TypeSend:
			d.mu.RLock()
			target, ok := d.agents[env.To]
			d.mu.RUnlock()
			if !ok {
				errBody, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("agent %d not found", env.To)})
				d.sendTo(conn, Envelope{Type: TypeError, Body: errBody})
				continue
			}
			fwd := Envelope{Type: TypeMessage, From: env.From, Body: env.Body}
			if err := d.sendTo(target.conn, fwd); err != nil {
				log.Printf("failed to forward to agent %d: %v", env.To, err)
				errBody, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("agent %d unreachable", env.To)})
				d.sendTo(conn, Envelope{Type: TypeError, Body: errBody})
				continue
			}
			d.sendTo(conn, Envelope{Type: TypeSent})

		}
	}
}

func (d *Daemon) sameProject(sender, target *Agent, all bool) bool {
	return all || sender == nil || sender.Project == "" || target.Project == sender.Project
}

func (d *Daemon) sendTo(conn net.Conn, env Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = conn.Write(data)
	return err
}

// EnsureRunning starts the daemon in the background if it isn't already listening.
// It blocks until the daemon is accepting connections.
func EnsureRunning() error {
	conn, err := net.Dial("tcp", Addr)
	if err == nil {
		conn.Close()
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Wait for the daemon to start accepting connections.
	for range 50 {
		time.Sleep(10 * time.Millisecond)
		conn, err := net.Dial("tcp", Addr)
		if err == nil {
			conn.Close()
			return nil
		}
	}
	return fmt.Errorf("daemon did not become ready")
}
