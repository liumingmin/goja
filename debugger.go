package goja

import (
	"bufio"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/unistring"
)

// Debugger is a VM-level debugger. It is created by Runtime.AttachDebugger and
// cooperates with the VM through activation channels: whenever the VM hits a
// breakpoint or a `debugger` statement it blocks until Continue() (or one of
// the step methods followed by Continue-style pumping) is called from another
// goroutine.
//
// Typical usage:
//
//	vm := goja.New()
//	dbg := vm.AttachDebugger()
//	go func() { vm.RunScript("test.js", src) }()
//	for {
//		reason := dbg.Continue()
//		...inspect dbg.Line(), dbg.Filename(), variables...
//		if reason == ... { break }
//	}
type Debugger struct {
	vm *vm

	done           chan struct{} // closed by the VM when the program ends
	doneOnce       sync.Once
	currentLine    int
	lastLine       int
	breakpoints    map[string][]int
	activationCh   chan chan ActivationReason
	currentCh      chan ActivationReason
	active         bool
	started        bool // set once the debug VM loop is running
	stepMode       stepMode
	stepStackDepth int
	lastBreakpoint struct {
		filename   string
		line       int
		stackDepth int
	}
}

type stepMode int

const (
	stepNone stepMode = iota
	stepIn
	stepOver
	stepOut
)

func newDebugger(vm *vm) *Debugger {
	dbg := &Debugger{
		vm:           vm,
		done:         make(chan struct{}),
		activationCh: make(chan chan ActivationReason),
		active:       false,
		breakpoints:  make(map[string][]int),
		lastLine:     0,
	}
	return dbg
}

// ActivationReason describes why the debugger got control.
type ActivationReason string

const (
	ProgramStartActivation      ActivationReason = "start"
	DebuggerStatementActivation ActivationReason = "debugger"
	BreakpointActivation        ActivationReason = "breakpoint"
	StepActivation              ActivationReason = "step"
)

var globalBuiltinKeys = map[string]bool{"Object": true, "Function": true, "Array": true, "String": true, "globalThis": true, "NaN": true, "undefined": true, "Infinity": true, "isNaN": true, "parseInt": true, "parseFloat": true, "isFinite": true, "decodeURI": true, "decodeURIComponent": true, "encodeURI": true, "encodeURIComponent": true, "escape": true, "unescape": true, "Number": true, "RegExp": true, "Date": true, "Boolean": true, "Proxy": true, "Reflect": true, "Error": true, "AggregateError": true, "TypeError": true, "ReferenceError": true, "SyntaxError": true, "RangeError": true, "EvalError": true, "URIError": true, "GoError": true, "eval": true, "Math": true, "JSON": true, "ArrayBuffer": true, "DataView": true, "Uint8Array": true, "Uint8ClampedArray": true, "Int8Array": true, "Uint16Array": true, "Int16Array": true, "Uint32Array": true, "Int32Array": true, "Float32Array": true, "Float64Array": true, "BigInt64Array": true, "BigUint64Array": true, "Symbol": true, "WeakSet": true, "WeakMap": true, "Map": true, "Set": true, "Promise": true, "BigInt": true, "FinalizationRegistry": true, "WeakRef": true}

func (dbg *Debugger) activate(reason ActivationReason) {
	dbg.active = true
	ch := <-dbg.activationCh // get channel from waiter
	ch <- reason             // send what activated it
	<-ch                     // wait for deactivation
	dbg.active = false
}

// Continue unblocks the goja runtime to run code as is and will return the
// reason why it blocked again. If the program has already finished it returns
// "" immediately instead of blocking forever.
func (dbg *Debugger) Continue() ActivationReason {
	if dbg.IsDone() {
		return ""
	}
	if dbg.currentCh != nil {
		close(dbg.currentCh)
	}
	dbg.currentCh = make(chan ActivationReason)
	select {
	case dbg.activationCh <- dbg.currentCh:
	case <-dbg.done:
		return ""
	}
	select {
	case reason, ok := <-dbg.currentCh:
		if !ok {
			return ""
		}
		return reason
	case <-dbg.done:
		return ""
	}
}

// PC returns the current program counter.
func (dbg *Debugger) PC() int {
	return dbg.vm.pc
}

// IsDone reports whether the program being debugged has run to completion
// (or the debugger has been detached). Drivers should stop pumping Continue()
// when this returns true. Before the program starts (or while it is paused at
// an activation) it reports false.
func (dbg *Debugger) IsDone() bool {
	select {
	case <-dbg.done:
		return true
	default:
		return dbg.vm == nil
	}
}

// signalDone is called by the VM when the program being debugged terminates.
func (dbg *Debugger) signalDone() {
	dbg.doneOnce.Do(func() { close(dbg.done) })
}

// Detach the debugger; after this call the instance must not be used.
// This also disables debug mode for the runtime.
func (dbg *Debugger) Detach() {
	dbg.vm.debugger = nil
	dbg.vm.debugMode = false
	dbg.vm = nil
	dbg.active = false
	if dbg.currentCh != nil {
		close(dbg.currentCh)
		dbg.currentCh = nil
	}
}

// SetBreakpoint adds a breakpoint at the given file and 1-based line.
func (dbg *Debugger) SetBreakpoint(filename string, line int) (err error) {
	idx := sort.SearchInts(dbg.breakpoints[filename], line)
	if idx < len(dbg.breakpoints[filename]) && dbg.breakpoints[filename][idx] == line {
		err = errors.New("breakpoint exists")
	} else {
		dbg.breakpoints[filename] = append(dbg.breakpoints[filename], line)
		if len(dbg.breakpoints[filename]) > 1 {
			sort.Ints(dbg.breakpoints[filename])
		}
	}
	return
}

// ClearBreakpoint removes a previously set breakpoint.
func (dbg *Debugger) ClearBreakpoint(filename string, line int) (err error) {
	if len(dbg.breakpoints[filename]) == 0 {
		return errors.New("no breakpoints")
	}

	idx := sort.SearchInts(dbg.breakpoints[filename], line)
	if idx < len(dbg.breakpoints[filename]) && dbg.breakpoints[filename][idx] == line {
		dbg.breakpoints[filename] = append(dbg.breakpoints[filename][:idx], dbg.breakpoints[filename][idx+1:]...)
		if len(dbg.breakpoints[filename]) == 0 {
			delete(dbg.breakpoints, filename)
		}
	} else {
		err = errors.New("breakpoint doesn't exist")
	}
	return
}

// ClearAllBreakpoints removes every breakpoint.
func (dbg *Debugger) ClearAllBreakpoints() {
	dbg.breakpoints = make(map[string][]int)
}

// Breakpoints returns all breakpoints indexed by file name.
func (dbg *Debugger) Breakpoints() (map[string][]int, error) {
	if len(dbg.breakpoints) == 0 {
		return nil, errors.New("no breakpoints")
	}

	return dbg.breakpoints, nil
}

// StepMode arms single-stepping; the VM will stop again at the next statement
// boundary according to the mode. Must be followed by Continue().
func (dbg *Debugger) StepMode(m string) {
	switch m {
	case "in":
		dbg.stepMode = stepIn
	case "over":
		dbg.stepMode = stepOver
	case "out":
		dbg.stepMode = stepOut
	default:
		dbg.stepMode = stepNone
	}
	dbg.stepStackDepth = dbg.callStackDepth()
}

// StepIn executes exactly one instruction.
func (dbg *Debugger) StepIn() error {
	lastLine := dbg.Line()
	dbg.updateCurrentLine()
	if dbg.safeToRun() {
		dbg.updateCurrentLine()
		dbg.vm.prg.code[dbg.vm.pc].exec(dbg.vm)
		dbg.updateLastLine(lastLine)
	} else if dbg.vm.halted() {
		return errors.New("halted")
	}
	return nil
}

// Next steps over to the next line in the current program.
func (dbg *Debugger) Next() error {
	lastLine := dbg.Line()
	dbg.updateCurrentLine()
	if dbg.getLastLine() != dbg.Line() {
		nextLine := dbg.getNextLine()
		for dbg.safeToRun() && nextLine > 0 && dbg.Line() != nextLine {
			dbg.updateCurrentLine()
			dbg.vm.prg.code[dbg.vm.pc].exec(dbg.vm)
		}
		dbg.updateLastLine(lastLine)
	} else if dbg.getNextLine() == 0 {
		return errors.New("exhausted")
	} else if dbg.vm.halted() {
		return errors.New("halted")
	}
	return nil
}

// Exec evaluates an expression in the context of the currently paused frame.
func (dbg *Debugger) Exec(expr string) (Value, error) {
	if expr == "" {
		return nil, errors.New("nothing to execute")
	}
	val, err := dbg.eval(expr)

	lastLine := dbg.Line()
	dbg.updateLastLine(lastLine)
	return val, err
}

// Print resolves a variable name and returns its string representation.
func (dbg *Debugger) Print(varName string) (string, error) {
	if varName == "" {
		return "", errors.New("please specify variable name")
	}
	val, err := dbg.getValue(varName)

	if val == Undefined() {
		return "undefined", err
	}
	return fmt.Sprint(val), err
}

// List returns the source lines of the currently executing program.
func (dbg *Debugger) List() ([]string, error) {
	return stringToLines(dbg.vm.prg.src.Source())
}

func stringToLines(s string) (lines []string, err error) {
	scanner := bufio.NewScanner(strings.NewReader(s))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	err = scanner.Err()
	return
}

func (dbg *Debugger) breakpoint() bool {
	filename := dbg.Filename()
	line := dbg.Line()

	idx := sort.SearchInts(dbg.breakpoints[filename], line)
	if idx < len(dbg.breakpoints[filename]) && dbg.breakpoints[filename][idx] == line {
		return true
	}
	return false
}

// shouldStep reports whether single-stepping should stop at the current position.
func (dbg *Debugger) shouldStep() bool {
	if dbg.stepMode == stepNone {
		return false
	}
	line := dbg.Line()
	depth := dbg.callStackDepth()
	switch dbg.stepMode {
	case stepIn:
		return line != dbg.currentLine || dbg.Filename() != dbg.lastBreakpoint.filename
	case stepOver:
		if depth > dbg.stepStackDepth {
			return false
		}
		return line != dbg.currentLine
	case stepOut:
		return depth < dbg.stepStackDepth
	}
	return false
}

func (dbg *Debugger) getLastLine() int {
	if dbg.lastLine >= 0 {
		return dbg.lastLine
	}
	return dbg.Line()
}

func (dbg *Debugger) updateLastLine(lineNumber int) {
	if dbg.lastLine != lineNumber {
		dbg.lastLine = lineNumber
	}
}

func (dbg *Debugger) callStackDepth() int {
	return len(dbg.vm.callStack)
}

// Line returns the 1-based source line of the instruction about to execute.
func (dbg *Debugger) Line() int {
	return dbg.vm.prg.src.Position(dbg.vm.prg.sourceOffset(dbg.vm.pc)).Line
}

// Column returns the 1-based source column of the instruction about to execute.
func (dbg *Debugger) Column() int {
	return dbg.vm.prg.src.Position(dbg.vm.prg.sourceOffset(dbg.vm.pc)).Column
}

// Filename returns the name of the currently executing source file.
func (dbg *Debugger) Filename() string {
	return dbg.vm.prg.src.Name()
}

// CallStack returns the captured Go stack of the JS call frames.
func (dbg *Debugger) CallStack() []StackFrame {
	return dbg.vm.captureStack(make([]StackFrame, 0, len(dbg.vm.callStack)+1), 0)
}

func (dbg *Debugger) updateCurrentLine() {
	dbg.currentLine = dbg.Line()
	dbg.lastBreakpoint.filename = dbg.Filename()
}

func (dbg *Debugger) getNextLine() int {
	for idx := range dbg.vm.prg.code[dbg.vm.pc:] {
		nextLine := dbg.vm.prg.src.Position(dbg.vm.prg.sourceOffset(dbg.vm.pc + idx + 1)).Line
		if nextLine > dbg.Line() {
			return nextLine
		}
	}
	return 0
}

func (dbg *Debugger) safeToRun() bool {
	return dbg.vm.pc < len(dbg.vm.prg.code)
}

func (dbg *Debugger) eval(expr string) (v Value, err error) {
	prg, err := parser.ParseFile(nil, "<eval>", expr, 0)
	if err != nil {
		return nil, &CompilerSyntaxError{
			CompilerError: CompilerError{
				Message: err.Error(),
			},
		}
	}

	c := newCompiler(true)

	defer func() {
		if x := recover(); x != nil {
			c.p = nil
			switch ex := x.(type) {
			case *CompilerSyntaxError:
				err = ex
			default:
				err = fmt.Errorf("cannot recover from exception %v", ex)
			}
		}
	}()

	var this Value
	if dbg.vm.sb >= 0 {
		this = dbg.vm.stack[dbg.vm.sb]
	} else {
		this = dbg.vm.r.globalObject
	}

	c.compile(prg, false, true, nil)

	defer func() {
		if x := recover(); x != nil {
			if ex, ok := x.(uncatchableException); ok {
				err = ex
			} else {
				err = fmt.Errorf("cannot recover from exception %v", x)
			}
		}
		dbg.vm.popCtx()
		dbg.vm.sp -= 1
	}()

	dbg.vm.pushCtx()
	dbg.vm.prg = c.p
	dbg.vm.pc = 0
	dbg.vm.args = 0
	dbg.vm.result = _undefined
	dbg.vm.sb = dbg.vm.sp
	dbg.vm.push(this)
	dbg.vm.run()
	v = dbg.vm.result
	return v, err
}

func (dbg *Debugger) getValue(varName string) (val Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			val = nil
			err = fmt.Errorf("cannot resolve %q", varName)
		}
	}()

	// copied from loadDynamicRef
	name := unistring.String(varName)
	for stash := dbg.vm.stash; stash != nil; stash = stash.outer {
		if v, exists := stash.getByName(name); exists {
			val = v
			break
		}
	}
	if val == nil {
		val = dbg.vm.r.globalObject.self.getStr(name, nil)
		if val == nil {
			val = valueUnresolved{r: dbg.vm.r, ref: name}
		}
	}
	return val, nil
}

// GetGlobalVariables returns user-defined global bindings (built-ins filtered out).
func (dbg *Debugger) GetGlobalVariables() (map[string]Value, error) {
	defer func() {
		_ = recover()
	}()

	globals := make(map[string]Value)
	for _, name := range dbg.vm.r.globalObject.self.stringKeys(true, nil) {
		nameStr := name.String()
		if globalBuiltinKeys[nameStr] {
			continue
		}
		val := dbg.vm.r.globalObject.self.getStr(unistring.String(nameStr), nil)
		if val != nil {
			globals[nameStr] = val
		}
	}
	return globals, nil
}

// GetLocalVariables returns the variables visible in the current stash chain.
func (dbg *Debugger) GetLocalVariables() (map[string]Value, error) {
	defer func() {
		_ = recover()
	}()

	locals := make(map[string]Value)
	for stash := dbg.vm.stash; stash != nil; stash = stash.outer {
		for name := range stash.names {
			nameStr := name.String()
			if _, dup := locals[nameStr]; dup {
				continue
			}
			if v, exists := stash.getByName(name); exists {
				locals[nameStr] = v
			}
		}
	}
	return locals, nil
}
