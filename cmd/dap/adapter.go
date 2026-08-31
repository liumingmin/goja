package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	goja "github.com/dop251/goja"
	"github.com/google/go-dap"
)

const mainThreadID = 1

// debugSession holds one DAP session: a goja runtime running the script on a
// dedicated goroutine, a Debugger driven from the DAP loop, and the plumbing
// between them.
type debugSession struct {
	scriptPath  string
	source      string
	stopOnEntry bool

	out          io.Writer
	outMu        sync.Mutex
	seq          int
	disconnected bool

	// sendQ decouples message production (dispatch loop, debugger pump,
	// console hooks) from the actual write, which may block on an unbuffered
	// pipe or a slow TCP peer. Without this, a write from the dispatch
	// goroutine can deadlock against a client that is itself writing.
	sendQ    chan dap.Message
	sendDone chan struct{}

	vm  *goja.Runtime
	dbg *goja.Debugger

	// debugger goroutine coordination
	resumeCh  chan string   // dap loop -> debugger ("continue"/"in"/"over"/"out")
	runDone   chan struct{} // closed when the script goroutine finishes
	runErr    error
	startOnce sync.Once

	varHandlesMu sync.Mutex
	varHandles   map[int]*varHandle
	nextHandle   int

	outputBuf strings.Builder
}

type stopInfo struct {
	reason string // dap "reason" for the stopped event
	descr  string
}

type varHandle struct {
	value  goja.Value
	expand func() []dap.Variable
}

func newDebugSession(scriptPath, source string, stopOnEntry bool) *debugSession {
	return &debugSession{
		scriptPath:  scriptPath,
		source:      source,
		stopOnEntry: stopOnEntry,
		resumeCh:    make(chan string),
		runDone:     make(chan struct{}),
		sendQ:       make(chan dap.Message, 256),
		sendDone:    make(chan struct{}),
		varHandles:  make(map[int]*varHandle),
		nextHandle:  1000,
	}
}

// ---------------------------------------------------------------------------
// DAP plumbing
// ---------------------------------------------------------------------------

// send queues a message for the writer goroutine; it never blocks the caller
// for longer than the queue depth allows.
func (s *debugSession) send(msg dap.Message) {
	select {
	case s.sendQ <- msg:
	case <-s.sendDone:
	}
}

// writeLoop is the single writer to the DAP stream.
func (s *debugSession) writeLoop() {
	defer close(s.sendDone)
	for msg := range s.sendQ {
		s.outMu.Lock()
		s.seq++
		s.writeOne(msg)
		s.outMu.Unlock()
	}
}

func (s *debugSession) writeOne(msg dap.Message) {
	// go-dap requires the "type" discriminator to be set for decoding on the
	// receiving side; fill it in here so individual handlers don't have to.
	switch m := msg.(type) {
	case dap.ResponseMessage:
		m.GetResponse().Type = "response"
		m.GetResponse().Seq = s.seq
		if m.GetResponse().Command == "" {
			// mirror the request command
			// (set by respond via req)
		}
	case dap.EventMessage:
		m.GetEvent().Type = "event"
		m.GetEvent().Seq = s.seq
	}
	err := dap.WriteProtocolMessage(s.out, msg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goja-dap: write error: %v\n", err)
	}
}

func (s *debugSession) respond(req *dap.Request, resp dap.ResponseMessage) {
	r := resp.GetResponse()
	r.RequestSeq = req.Seq
	r.Command = req.Command
	s.send(resp)
}

func (s *debugSession) respondErr(req *dap.Request, resp dap.ResponseMessage, msg string) {
	r := resp.GetResponse()
	r.Success = false
	r.Message = msg
	s.respond(req, resp)
}

func (s *debugSession) event(ev dap.EventMessage) {
	s.send(ev)
}

// ---------------------------------------------------------------------------
// dispatch
// ---------------------------------------------------------------------------

func (s *debugSession) dispatch(req dap.RequestMessage) {
	if r, ok := req.(*dap.Request); ok {
		// go-dap decoded a command it has no concrete type for.
		s.dispatchGeneric(r)
		return
	}
	switch r := req.(type) {
	case *dap.InitializeRequest:
		s.respond(req.GetRequest(), &dap.InitializeResponse{
			Response: newOKResponse(),
			Body: dap.Capabilities{
				SupportsConfigurationDoneRequest: true,
				SupportsEvaluateForHovers:        true,
				SupportsStepBack:                 false,
				SupportsSetVariable:              false,
				SupportsRestartRequest:           false,
				SupportsTerminateRequest:         true,
				SupportTerminateDebuggee:         true,
			},
		})
		s.event(&dap.InitializedEvent{Event: newEvent("initialized")})

	case *dap.LaunchRequest, *dap.AttachRequest:
		switch r.(type) {
		case *dap.LaunchRequest:
			s.respond(req.GetRequest(), &dap.LaunchResponse{Response: newOKResponse()})
		case *dap.AttachRequest:
			s.respond(req.GetRequest(), &dap.AttachResponse{Response: newOKResponse()})
		}
		// wait for configurationDone before running
	case *dap.ConfigurationDoneRequest:
		s.respond(req.GetRequest(), &dap.ConfigurationDoneResponse{Response: newOKResponse()})
		s.start()

	case *dap.SetBreakpointsRequest:
		s.onSetBreakpoints(req.GetRequest(), r)

	case *dap.ThreadsRequest:
		s.respond(req.GetRequest(), &dap.ThreadsResponse{
			Response: newOKResponse(),
			Body: dap.ThreadsResponseBody{
				Threads: []dap.Thread{{Id: mainThreadID, Name: "main"}},
			},
		})

	case *dap.StackTraceRequest:
		s.onStackTrace(req.GetRequest(), r)
	case *dap.ScopesRequest:
		s.onScopes(req.GetRequest(), r)
	case *dap.VariablesRequest:
		s.onVariables(req.GetRequest(), r)
	case *dap.EvaluateRequest:
		s.onEvaluate(req.GetRequest(), r)

	case *dap.ContinueRequest:
		s.respond(req.GetRequest(), &dap.ContinueResponse{
			Response: newOKResponse(),
			Body:     dap.ContinueResponseBody{AllThreadsContinued: true},
		})
		s.resumeCh <- "continue"
		s.event(&dap.ContinuedEvent{
			Event: newEvent("continued"),
			Body:  dap.ContinuedEventBody{ThreadId: mainThreadID, AllThreadsContinued: true},
		})

	case *dap.NextRequest:
		s.respond(req.GetRequest(), &dap.NextResponse{Response: newOKResponse()})
		s.resumeCh <- "over"
	case *dap.StepInRequest:
		s.respond(req.GetRequest(), &dap.StepInResponse{Response: newOKResponse()})
		s.resumeCh <- "in"
	case *dap.StepOutRequest:
		s.respond(req.GetRequest(), &dap.StepOutResponse{Response: newOKResponse()})
		s.resumeCh <- "out"
	case *dap.PauseRequest:
		// not supported: goja debugger activates synchronously
		s.respondErr(req.GetRequest(), &dap.PauseResponse{Response: newOKResponse()}, "pause not supported")

	case *dap.DisconnectRequest:
		s.respond(req.GetRequest(), &dap.DisconnectResponse{Response: newOKResponse()})
		s.disconnected = true
	case *dap.TerminateRequest:
		s.respond(req.GetRequest(), &dap.TerminateResponse{Response: newOKResponse()})
		s.disconnected = true

	case *dap.SetFunctionBreakpointsRequest:
		s.respond(req.GetRequest(), &dap.SetFunctionBreakpointsResponse{Response: newOKResponse()})
	case *dap.SetExceptionBreakpointsRequest:
		s.respond(req.GetRequest(), &dap.SetExceptionBreakpointsResponse{Response: newOKResponse()})
	case *dap.SourceRequest:
		s.respond(req.GetRequest(), &dap.SourceResponse{
			Response: newOKResponse(),
			Body:     dap.SourceResponseBody{Content: s.source},
		})
	default:
		// Unknown / unsupported: reply with an error but keep going.
		s.respondErr(req.GetRequest(), &dap.ErrorResponse{
			Response: newOKResponse(),
			Body:     dap.ErrorResponseBody{Error: &dap.ErrorMessage{Format: "unsupported request " + req.GetRequest().Command}},
		}, "unsupported request "+req.GetRequest().Command)
	}
}

// ---------------------------------------------------------------------------
// execution engine
// ---------------------------------------------------------------------------

// start launches the script goroutine and the debugger pump. Idempotent.
// The debugger itself is created eagerly in ensureDebugger so that breakpoint
// requests can be handled before the script starts (per DAP spec,
// setBreakpoints arrives before configurationDone).
func (s *debugSession) start() {
	s.ensureDebugger()
	s.startOnce.Do(func() {
		go func() {
			defer close(s.runDone)
			defer func() {
				if r := recover(); r != nil {
					s.runErr = fmt.Errorf("script panicked: %v", r)
				}
			}()
			_, err := s.vm.RunScript(filepath.Base(s.scriptPath), s.source)
			s.runErr = err
			// Unblock the pump if it is waiting for an activation.
			s.dbg.Detach()
		}()

		go s.pump()
	})
}

func (s *debugSession) ensureDebugger() {
	if s.dbg != nil {
		return
	}
	s.vm = goja.New()
	s.installConsole()
	s.dbg = s.vm.AttachDebugger()
}

// pump is the debugger-driving loop: it repeatedly calls Continue() (which
// blocks until the next activation or program end) and surfaces stop events.
func (s *debugSession) pump() {
	for {
		reason := s.dbg.Continue()
		if s.dbg.IsDone() || reason == "" {
			// program finished
			<-s.runDone
			if s.runErr != nil {
				s.emitOutput("stderr", fmt.Sprintf("Uncaught %v\n", s.runErr))
			}
			s.flushOutput()
			s.event(&dap.ThreadEvent{Event: newEvent("thread"), Body: dap.ThreadEventBody{Reason: "exited", ThreadId: mainThreadID}})
			s.event(&dap.TerminatedEvent{Event: newEvent("terminated")})
			return
		}

		var stop stopInfo
		switch reason {
		case goja.BreakpointActivation:
			stop = stopInfo{reason: "breakpoint", descr: "breakpoint hit"}
		case goja.DebuggerStatementActivation:
			stop = stopInfo{reason: "debugger statement", descr: "debugger statement"}
		case goja.StepActivation:
			stop = stopInfo{reason: "step", descr: "step finished"}
		default:
			stop = stopInfo{reason: string(reason)}
		}

		s.resetVarHandles()
		s.flushOutput()
		s.event(&dap.StoppedEvent{
			Event: newEvent("stopped"),
			Body: dap.StoppedEventBody{
				Reason:            stop.reason,
				Description:       stop.descr,
				ThreadId:          mainThreadID,
				AllThreadsStopped: true,
			},
		})

		// Wait for the DAP loop to tell us how to resume.
		mode := <-s.resumeCh
		if mode != "continue" {
			s.dbg.StepMode(mode)
		} else {
			s.dbg.StepMode("")
		}
	}
}

// stop tears the session down (called on disconnect/EOF).
func (s *debugSession) stop() {
	select {
	case s.resumeCh <- "continue":
	default:
	}
	if s.dbg != nil {
		// best effort; may already be detached
		func() {
			defer func() { _ = recover() }()
			s.dbg.Detach()
		}()
	}
}

// ---------------------------------------------------------------------------
// breakpoints
// ---------------------------------------------------------------------------

func (s *debugSession) onSetBreakpoints(req *dap.Request, r *dap.SetBreakpointsRequest) {
	s.ensureDebugger()
	name := r.Arguments.Source.Name
	if name == "" {
		name = filepath.Base(r.Arguments.Source.Path)
	}

	// Clear existing breakpoints for this file.
	if bps, err := s.dbg.Breakpoints(); err == nil {
		for _, l := range bps[name] {
			_ = s.dbg.ClearBreakpoint(name, l)
		}
	}

	out := make([]dap.Breakpoint, 0, len(r.Arguments.Breakpoints))
	for _, bp := range r.Arguments.Breakpoints {
		err := s.dbg.SetBreakpoint(name, bp.Line)
		out = append(out, dap.Breakpoint{
			Verified: err == nil,
			Line:     bp.Line,
			Message:  errString(err),
		})
	}
	s.respond(req.GetRequest(), &dap.SetBreakpointsResponse{
		Response: newOKResponse(),
		Body:     dap.SetBreakpointsResponseBody{Breakpoints: out},
	})
}

// ---------------------------------------------------------------------------
// stack / scopes / variables
// ---------------------------------------------------------------------------

func (s *debugSession) onStackTrace(req *dap.Request, r *dap.StackTraceRequest) {
	frames := s.dbg.CallStack()
	dapFrames := make([]dap.StackFrame, 0, len(frames)+1)

	// top frame: current position
	pos := s.currentPosition()
	dapFrames = append(dapFrames, dap.StackFrame{
		Id:     frameID(0),
		Name:   topFrameName(s.dbg),
		Line:   pos.Line,
		Column: pos.Column,
		Source: &dap.Source{Name: filepath.Base(s.scriptPath), Path: s.scriptPath},
	})
	for i := range frames {
		f := frames[len(frames)-1-i] // goja reports caller->callee? reverse to innermost-first
		p := f.Position()
		name := f.FuncName()
		if name == "" {
			name = "<anonymous>"
		}
		dapFrames = append(dapFrames, dap.StackFrame{
			Id:     frameID(i + 1),
			Name:   name,
			Line:   p.Line,
			Column: p.Column,
			Source: &dap.Source{Name: f.SrcName(), Path: f.SrcName()},
		})
	}

	s.respond(req.GetRequest(), &dap.StackTraceResponse{
		Response: newOKResponse(),
		Body:     dap.StackTraceResponseBody{StackFrames: dapFrames, TotalFrames: len(dapFrames)},
	})
}

func frameID(i int) int { return 100 + i }

func topFrameName(dbg *goja.Debugger) string {
	frames := dbg.CallStack()
	if len(frames) > 0 {
		if n := frames[len(frames)-1].FuncName(); n != "" {
			return n
		}
	}
	return "<program>"
}

type position struct{ Line, Column int }

func (s *debugSession) currentPosition() position {
	func() {
		defer func() { _ = recover() }()
	}()
	return position{Line: s.dbg.Line(), Column: s.dbg.Column()}
}

func (s *debugSession) onScopes(req *dap.Request, r *dap.ScopesRequest) {
	localsRef := s.newVarHandle(&varHandle{
		expand: func() []dap.Variable {
			vars, err := s.dbg.GetLocalVariables()
			if err != nil {
				return nil
			}
			return s.mapToVars(vars)
		},
	})
	globalsRef := s.newVarHandle(&varHandle{
		expand: func() []dap.Variable {
			vars, err := s.dbg.GetGlobalVariables()
			if err != nil {
				return nil
			}
			return s.mapToVars(vars)
		},
	})

	s.respond(req.GetRequest(), &dap.ScopesResponse{
		Response: newOKResponse(),
		Body: dap.ScopesResponseBody{Scopes: []dap.Scope{
			{Name: "Local", VariablesReference: localsRef, Expensive: false},
			{Name: "Global", VariablesReference: globalsRef, Expensive: false},
		}},
	})
}

func (s *debugSession) onVariables(req *dap.Request, r *dap.VariablesRequest) {
	s.varHandlesMu.Lock()
	h := s.varHandles[r.Arguments.VariablesReference]
	s.varHandlesMu.Unlock()
	var vars []dap.Variable
	if h != nil && h.expand != nil {
		vars = h.expand()
	}
	s.respond(req.GetRequest(), &dap.VariablesResponse{
		Response: newOKResponse(),
		Body:     dap.VariablesResponseBody{Variables: vars},
	})
}

func (s *debugSession) mapToVars(m map[string]goja.Value) []dap.Variable {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]dap.Variable, 0, len(names))
	for _, n := range names {
		out = append(out, s.valueToVar(n, m[n]))
	}
	return out
}

// valueToVar renders a goja.Value as a DAP Variable; objects get a
// variablesReference so the client can expand them lazily.
func (s *debugSession) valueToVar(name string, v goja.Value) dap.Variable {
	variable := dap.Variable{Name: name, Value: renderValue(v), Type: typeOf(v)}
	if obj, ok := v.(*goja.Object); ok {
		ref := s.newVarHandle(&varHandle{
			value: v,
			expand: func() []dap.Variable {
				return s.objectVars(obj)
			},
		})
		variable.VariablesReference = ref
	}
	return variable
}

func (s *debugSession) objectVars(obj *goja.Object) []dap.Variable {
	var out []dap.Variable
	func() {
		defer func() { _ = recover() }()
		for _, k := range obj.Keys() {
			out = append(out, s.valueToVar(k, obj.Get(k)))
		}
	}()
	return out
}

func (s *debugSession) newVarHandle(h *varHandle) int {
	s.varHandlesMu.Lock()
	defer s.varHandlesMu.Unlock()
	s.nextHandle++
	s.varHandles[s.nextHandle] = h
	return s.nextHandle
}

func (s *debugSession) resetVarHandles() {
	s.varHandlesMu.Lock()
	defer s.varHandlesMu.Unlock()
	s.varHandles = make(map[int]*varHandle)
	s.nextHandle = 1000
}

// ---------------------------------------------------------------------------
// evaluate
// ---------------------------------------------------------------------------

func (s *debugSession) onEvaluate(req *dap.Request, r *dap.EvaluateRequest) {
	val, err := s.dbg.Exec(r.Arguments.Expression)
	if err != nil {
		s.respondErr(req.GetRequest(), &dap.EvaluateResponse{Response: newOKResponse()}, err.Error())
		return
	}
	variable := s.valueToVar("result", val)
	s.respond(req.GetRequest(), &dap.EvaluateResponse{
		Response: newOKResponse(),
		Body: dap.EvaluateResponseBody{
			Result:             variable.Value,
			Type:               variable.Type,
			VariablesReference: variable.VariablesReference,
		},
	})
}

// ---------------------------------------------------------------------------
// console
// ---------------------------------------------------------------------------

func (s *debugSession) installConsole() {
	console := s.vm.NewObject()
	logFn := func(level string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			parts := make([]string, 0, len(call.Arguments))
			for _, a := range call.Arguments {
				parts = append(parts, renderValue(a))
			}
			s.emitOutput(level, strings.Join(parts, " ")+"\n")
			return goja.Undefined()
		}
	}
	_ = console.Set("log", logFn("stdout"))
	_ = console.Set("info", logFn("stdout"))
	_ = console.Set("warn", logFn("console"))
	_ = console.Set("error", logFn("stderr"))
	_ = console.Set("debug", logFn("console"))
	_ = s.vm.Set("console", console)
	_ = s.vm.Set("print", logFn("stdout"))
}

func (s *debugSession) emitOutput(category, text string) {
	s.outputBuf.WriteString(text)
	// stream immediately as well
	s.event(&dap.OutputEvent{
		Event: newEvent("output"),
		Body:  dap.OutputEventBody{Category: category, Output: text},
	})
}

func (s *debugSession) flushOutput() { s.outputBuf.Reset() }

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newOKResponse() dap.Response {
	return dap.Response{Success: true}
}

func newEvent(name string) dap.Event {
	return dap.Event{Event: name}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func typeOf(v goja.Value) string {
	if v == nil {
		return "undefined"
	}
	switch v.ExportType() {
	case nil:
		return v.String()
	}
	t := v.ExportType()
	if t == nil {
		return "object"
	}
	return t.Kind().String()
}

func renderValue(v goja.Value) (s string) {
	defer func() {
		if r := recover(); r != nil {
			s = fmt.Sprintf("<error: %v>", r)
		}
	}()
	if v == nil {
		return "nil"
	}
	if str, ok := v.(interface{ String() string }); ok {
		if obj, isObj := v.(*goja.Object); isObj && obj != nil {
			if obj.ClassName() == "Object" || obj.ClassName() == "Array" {
				if j, err := jsonMarshal(v.Export()); err == nil {
					return j
				}
			}
		}
		return str.String()
	}
	return fmt.Sprint(v)
}

func jsonMarshal2(m dap.Message) ([]byte, error) {
	return json.Marshal(m)
}

func jsonMarshal(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

var _ = log.Printf
