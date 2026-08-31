// Command goja-dap is a Debug Adapter Protocol server for goja.
//
// It speaks DAP over a TCP socket and lets any DAP client (VS Code via a
// thin extension, or any generic DAP client) debug JavaScript running in the
// embedded goja runtime.
//
// Usage:
//
//	goja-dap [flags] <script.js>
//
// Flags:
//
//	-port int    TCP port to listen on (default 5678)
//	-stdio       speak DAP on stdin/stdout instead of TCP (for editor-managed launches)
//	-stopOnEntry stop on the first statement
//
// VS Code attach configuration (with the "Debugger for goja" extension or any
// generic DAP extension):
//
//	{
//	  "type": "goja",
//	  "request": "attach",
//	  "name": "Attach to goja",
//	  "debugServer": 5678
//	}
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/google/go-dap"
)

func main() {
	port := flag.Int("port", 5678, "TCP port to listen on")
	useStdio := flag.Bool("stdio", false, "use stdin/stdout instead of TCP")
	stopOnEntry := flag.Bool("stopOnEntry", false, "stop on the first statement")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: goja-dap [flags] <script.js>")
		flag.PrintDefaults()
		os.Exit(2)
	}

	scriptPath, err := filepath.Abs(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	src, err := os.ReadFile(scriptPath)
	if err != nil {
		log.Fatalf("cannot read %s: %v", scriptPath, err)
	}

	session := newDebugSession(scriptPath, string(src), *stopOnEntry)

	if *useStdio {
		log.Printf("goja-dap: serving %s on stdio", scriptPath)
		if err := session.serve(os.Stdin, os.Stdout); err != nil && err != io.EOF {
			log.Printf("session ended: %v", err)
		}
		return
	}

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("goja-dap: serving %s on %s (DAP over TCP)", scriptPath, addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("client connected: %s", conn.RemoteAddr())
		// One client at a time; the script runs once per connection.
		s := newDebugSession(scriptPath, string(src), *stopOnEntry)
		if err := s.serve(conn, conn); err != nil && err != io.EOF {
			log.Printf("session ended: %v", err)
		}
		conn.Close()
		log.Printf("client disconnected")
	}
}

// serve runs the DAP read/dispatch loop until disconnect or EOF.
func (s *debugSession) serve(r io.Reader, w io.Writer) error {
	s.out = w
	go s.writeLoop()
	defer close(s.sendQ)
	br := bufio.NewReader(r)
	for {
		msg, err := dap.ReadProtocolMessage(br)
		if err != nil {
			s.stop()
			return err
		}
		switch req := msg.(type) {
		case dap.RequestMessage:
			s.dispatch(req)
		case *dap.Request:
			// go-dap decodes requests whose command it doesn't recognize as
			// the generic *dap.Request. Re-dispatch on the command string.
			s.dispatchGeneric(req)
		}
		if s.disconnected {
			s.stop()
			return nil
		}
	}
}

// dispatchGeneric handles requests decoded as generic *dap.Request (go-dap
// doesn't have a concrete type for every command).
func (s *debugSession) dispatchGeneric(req *dap.Request) {
	switch req.Command {
	case "initialize":
		s.respond(req, &dap.InitializeResponse{
			Response: newOKResponse(),
			Body: dap.Capabilities{
				SupportsConfigurationDoneRequest: true,
				SupportsEvaluateForHovers:        true,
				SupportsTerminateRequest:         true,
				SupportTerminateDebuggee:         true,
			},
		})
		s.event(&dap.InitializedEvent{Event: newEvent("initialized")})
	default:
		s.respondErr(req, &dap.ErrorResponse{
			Response: newOKResponse(),
			Body:     dap.ErrorResponseBody{Error: &dap.ErrorMessage{Format: "unsupported request " + req.Command}},
		}, "unsupported request "+req.Command)
	}
}
