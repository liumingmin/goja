package main

// Smoke test: drives the DAP adapter in-process (no TCP) through a full
// session: initialize -> attach -> setBreakpoints -> configurationDone ->
// breakpoint stop -> stackTrace -> scopes/variables -> evaluate -> step over ->
// continue -> debugger statement -> continue -> terminated.

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-dap"
)

// pipeSession wires a debugSession to an in-memory bidirectional pipe and
// provides request/response helpers.
type pipeSession struct {
	t         *testing.T
	clientW   *io.PipeWriter
	serverR   *io.PipeReader
	br        *bufio.Reader
	seq       int
	sess      *debugSession
	stopped   chan dap.StoppedEventBody
	termCh    chan struct{}
	closed    chan struct{}
	outputMu  sync.Mutex
	output    []string
	pendingMu sync.Mutex
	pending   []dap.Message
	pendingCh chan struct{}
}

func newPipeSession(t *testing.T, script string) *pipeSession {
	t.Helper()
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(script)

	ps := &pipeSession{
		t:         t,
		stopped:   make(chan dap.StoppedEventBody, 16),
		termCh:    make(chan struct{}, 1),
		closed:    make(chan struct{}),
		pendingCh: make(chan struct{}, 256),
	}
	clientR, clientW := io.Pipe() // client -> server
	serverR, serverW := io.Pipe() // server -> client
	ps.clientW = clientW
	ps.serverR = serverR
	ps.br = bufio.NewReader(serverR)

	ps.sess = newDebugSession(abs, string(src), false)
	go func() {
		_ = ps.sess.serve(clientR, serverW)
	}()
	go ps.readLoop()
	return ps
}

// readLoop is the single consumer of the server stream. Events are pushed to
// channels; responses go to the pending buffer.
func (ps *pipeSession) readLoop() {
	for {
		msg, err := dap.ReadProtocolMessage(ps.br)
		if err != nil {
			close(ps.closed)
			return
		}
		switch m := msg.(type) {
		case *dap.StoppedEvent:
			ps.stopped <- m.Body
		case *dap.TerminatedEvent:
			select {
			case ps.termCh <- struct{}{}:
			default:
			}
		case *dap.OutputEvent:
			ps.outputMu.Lock()
			ps.output = append(ps.output, m.Body.Output)
			ps.outputMu.Unlock()
		default:
			ps.pendingMu.Lock()
			ps.pending = append(ps.pending, msg)
			ps.pendingMu.Unlock()
			// wake any waiter
			select {
			case ps.pendingCh <- struct{}{}:
			default:
			}
		}
	}
}

func (ps *pipeSession) request(req dap.RequestMessage) dap.ResponseMessage {
	ps.t.Helper()
	ps.seq++
	req.GetRequest().Seq = ps.seq
	wantSeq := ps.seq
	if err := dap.WriteProtocolMessage(ps.clientW, req); err != nil {
		ps.t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ps.pendingMu.Lock()
		for i, m := range ps.pending {
			if resp, ok := m.(dap.ResponseMessage); ok && resp.GetResponse().RequestSeq == wantSeq {
				ps.pending = append(ps.pending[:i], ps.pending[i+1:]...)
				ps.pendingMu.Unlock()
				return resp
			}
		}
		ps.pendingMu.Unlock()
		select {
		case <-ps.pendingCh:
		case <-ps.closed:
			ps.t.Fatalf("connection closed waiting for response to seq %d", wantSeq)
		case <-time.After(time.Second):
		}
	}
	ps.t.Fatalf("timeout waiting for response to seq %d", wantSeq)
	return nil
}

func (ps *pipeSession) waitStopped() dap.StoppedEventBody {
	ps.t.Helper()
	select {
	case b := <-ps.stopped:
		return b
	case <-ps.closed:
		ps.t.Fatal("connection closed waiting for stopped event")
	case <-time.After(15 * time.Second):
		ps.t.Fatal("timeout waiting for stopped event")
	}
	return dap.StoppedEventBody{}
}

func (ps *pipeSession) waitTerminated() {
	ps.t.Helper()
	select {
	case <-ps.termCh:
	case <-ps.closed:
		ps.t.Fatal("connection closed waiting for terminated event")
	case <-time.After(15 * time.Second):
		ps.t.Fatal("timeout waiting for terminated event")
	}
}

func (ps *pipeSession) consoleOutput() string {
	ps.outputMu.Lock()
	defer ps.outputMu.Unlock()
	return strings.Join(ps.output, "")
}

func TestDAPSmoke(t *testing.T) {
	script := filepath.Join("testdata", "sample.js")
	abs, _ := filepath.Abs(script)

	ps := newPipeSession(t, script)

	// initialize
	resp := ps.request(&dap.InitializeRequest{
		Request: dap.Request{ProtocolMessage: dap.ProtocolMessage{Seq: 0, Type: "request"}, Command: "initialize"},
	})
	if !resp.GetResponse().Success {
		t.Fatal("initialize failed")
	}

	// attach
	resp = ps.request(&dap.AttachRequest{Request: dap.Request{ProtocolMessage: dap.ProtocolMessage{Type: "request"}, Command: "attach"}})
	if !resp.GetResponse().Success {
		t.Fatal("attach failed")
	}

	// set a breakpoint on the "var z = add(x, y)" line — locate it dynamically
	src, _ := os.ReadFile(script)
	bpLine := 0
	for i, l := range strings.Split(string(src), "\n") {
		if strings.Contains(l, "var z = add(x, y)") {
			bpLine = i + 1
			break
		}
	}
	if bpLine == 0 {
		t.Fatal("could not locate breakpoint line in sample.js")
	}

	resp = ps.request(&dap.SetBreakpointsRequest{
		Request: dap.Request{ProtocolMessage: dap.ProtocolMessage{Type: "request"}, Command: "setBreakpoints"},
		Arguments: dap.SetBreakpointsArguments{
			Source:      dap.Source{Name: filepath.Base(script), Path: abs},
			Breakpoints: []dap.SourceBreakpoint{{Line: bpLine}},
		},
	})
	bpResp := resp.(*dap.SetBreakpointsResponse)
	if len(bpResp.Body.Breakpoints) != 1 || !bpResp.Body.Breakpoints[0].Verified {
		t.Fatalf("breakpoint not verified: %+v", bpResp.Body.Breakpoints)
	}

	// configurationDone starts the script
	ps.request(&dap.ConfigurationDoneRequest{Request: dap.Request{ProtocolMessage: dap.ProtocolMessage{Type: "request"}, Command: "configurationDone"}})

	// expect a breakpoint stop
	stop := ps.waitStopped()
	if stop.Reason != "breakpoint" {
		t.Fatalf("expected breakpoint stop, got %q", stop.Reason)
	}

	// stackTrace
	resp = ps.request(&dap.StackTraceRequest{
		Request:   dap.Request{ProtocolMessage: dap.ProtocolMessage{Type: "request"}, Command: "stackTrace"},
		Arguments: dap.StackTraceArguments{ThreadId: mainThreadID},
	})
	stResp := resp.(*dap.StackTraceResponse)
	if len(stResp.Body.StackFrames) == 0 {
		t.Fatal("empty stack trace")
	}
	top := stResp.Body.StackFrames[0]
	if top.Line != bpLine {
		t.Fatalf("expected top frame at line %d, got %d (frames: %+v)", bpLine, top.Line, stResp.Body.StackFrames)
	}

	// scopes -> local variables: x and y must be visible with values 1 and 2
	resp = ps.request(&dap.ScopesRequest{
		Request:   dap.Request{ProtocolMessage: dap.ProtocolMessage{Type: "request"}, Command: "scopes"},
		Arguments: dap.ScopesArguments{FrameId: top.Id},
	})
	scopes := resp.(*dap.ScopesResponse).Body.Scopes
	if len(scopes) < 2 {
		t.Fatalf("expected at least 2 scopes, got %d", len(scopes))
	}

	resp = ps.request(&dap.VariablesRequest{
		Request:   dap.Request{ProtocolMessage: dap.ProtocolMessage{Type: "request"}, Command: "variables"},
		Arguments: dap.VariablesArguments{VariablesReference: scopes[0].VariablesReference},
	})
	vars := resp.(*dap.VariablesResponse).Body.Variables
	found := map[string]string{}
	for _, v := range vars {
		found[v.Name] = v.Value
	}
	if found["x"] != "1" || found["y"] != "2" {
		t.Fatalf("expected x=1 y=2 in locals, got %v", found)
	}

	// evaluate in the paused frame
	resp = ps.request(&dap.EvaluateRequest{
		Request:   dap.Request{ProtocolMessage: dap.ProtocolMessage{Type: "request"}, Command: "evaluate"},
		Arguments: dap.EvaluateArguments{Expression: "x + y + 100"},
	})
	if resp.(*dap.EvaluateResponse).Body.Result != "103" {
		t.Fatalf("evaluate x+y+100 = %q, want 103", resp.(*dap.EvaluateResponse).Body.Result)
	}

	// step over -> stop on the console.log line after add() returns
	ps.request(&dap.NextRequest{
		Request:   dap.Request{ProtocolMessage: dap.ProtocolMessage{Type: "request"}, Command: "next"},
		Arguments: dap.NextArguments{ThreadId: mainThreadID},
	})
	stop = ps.waitStopped()
	if stop.Reason != "step" {
		t.Fatalf("expected step stop after next, got %q", stop.Reason)
	}

	// z should now be 3
	resp = ps.request(&dap.EvaluateRequest{
		Request:   dap.Request{ProtocolMessage: dap.ProtocolMessage{Type: "request"}, Command: "evaluate"},
		Arguments: dap.EvaluateArguments{Expression: "z"},
	})
	if resp.(*dap.EvaluateResponse).Body.Result != "3" {
		t.Fatalf("after step over, z = %q, want 3", resp.(*dap.EvaluateResponse).Body.Result)
	}

	// continue -> hits the `debugger` statement
	ps.request(&dap.ContinueRequest{
		Request:   dap.Request{ProtocolMessage: dap.ProtocolMessage{Type: "request"}, Command: "continue"},
		Arguments: dap.ContinueArguments{ThreadId: mainThreadID},
	})
	stop = ps.waitStopped()
	if stop.Reason != "debugger statement" {
		t.Fatalf("expected debugger statement stop, got %q", stop.Reason)
	}

	// continue -> program ends
	ps.request(&dap.ContinueRequest{
		Request:   dap.Request{ProtocolMessage: dap.ProtocolMessage{Type: "request"}, Command: "continue"},
		Arguments: dap.ContinueArguments{ThreadId: mainThreadID},
	})
	ps.waitTerminated()

	// console.log output must have been streamed
	joined := ps.consoleOutput()
	if !strings.Contains(joined, "z = 3") {
		t.Fatalf("console output missing 'z = 3': %q", joined)
	}
	if !strings.Contains(joined, "result = 6") {
		t.Fatalf("console output missing 'result = 6': %q", joined)
	}
}
