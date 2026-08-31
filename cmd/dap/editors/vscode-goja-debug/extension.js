// Minimal goja debug extension: no build step, plain CommonJS.
//
// - launch: spawns `goja-dap -port <port> <program>` and connects over TCP
// - attach: connects to an already-running `goja-dap -port <port>` server
//
// VS Code's built-in DAP client handles the protocol; this extension only
// provides the transport (a socket).
//
// Diagnostics: everything is logged to the "goja-debug" output channel
// (View > Output > goja-debug).

const vscode = require("vscode");
const cp = require("child_process");
const net = require("net");

const output = vscode.window.createOutputChannel("goja-debug");

/** @type {cp.ChildProcess | undefined} */
let adapterProc;

function activate(context) {
  output.appendLine("goja-debug: activated");
  const factory = new GojaAdapterFactory();
  context.subscriptions.push(
    vscode.debug.registerDebugAdapterDescriptorFactory("goja", factory),
    vscode.debug.registerDebugAdapterTrackerFactory("goja", {
      createDebugAdapterTracker() {
        return {
          onWillReceiveMessage: (m) =>
            output.appendLine(">> " + JSON.stringify(m).slice(0, 300)),
          onDidSendMessage: (m) =>
            output.appendLine("<< " + JSON.stringify(m).slice(0, 300)),
          onError: (e) => output.appendLine("ERROR " + e.message),
          onExit: (code, signal) =>
            output.appendLine(`adapter exit code=${code} signal=${signal}`),
        };
      },
    }),
    vscode.debug.onDidTerminateDebugSession((s) => {
      output.appendLine("session terminated: " + s.name);
      if (adapterProc) {
        adapterProc.kill();
        adapterProc = undefined;
      }
    }),
    output
  );
}

class GojaAdapterFactory {
  /**
   * @param {vscode.DebugSession} session
   * @returns {vscode.ProviderResult<vscode.DebugAdapterDescriptor>}
   */
  createDebugAdapterDescriptor(session) {
    const cfg = session.configuration;
    const port = cfg.port || 5678;
    // session.request can be undefined on some VS Code versions when the
    // factory is consulted before the session is fully initialized; the
    // authoritative value is in configuration.request.
    const request = session.request || cfg.request || "launch";
    output.appendLine(
      `createDebugAdapterDescriptor request=${request} program=${cfg.program} port=${port}`
    );

    if (request === "launch") {
      let program = cfg.program;
      // ${file} resolves to whatever editor tab is active — if the user
      // pressed F5 while launch.json (or any non-JS file) is focused, that
      // file gets passed as the script and nothing happens. Guard against it.
      if (program && !/\.m?[jt]sx?$/i.test(program)) {
        const active = vscode.window.activeTextEditor?.document?.fileName;
        output.appendLine(
          `program "${program}" is not a JS file; active editor: ${active}`
        );
        if (active && /\.m?[jt]sx?$/i.test(active)) {
          program = active;
          output.appendLine(`falling back to active editor: ${program}`);
        } else {
          const msg =
            `goja: please open the JS file you want to debug first ` +
            `(got program="${program}"). Tip: click into the .js editor tab, then press F5.`;
          output.appendLine(msg);
          vscode.window.showErrorMessage(msg);
          return undefined;
        }
      }
      if (!program) {
        const msg = "goja launch: 'program' is required";
        output.appendLine(msg);
        vscode.window.showErrorMessage(msg);
        return undefined;
      }
      const bin = cfg.gojaDapPath || "goja-dap";
      const args = ["-port", String(port)];
      if (cfg.stopOnEntry) {
        args.push("-stopOnEntry");
      }
      args.push(program);
      output.appendLine(`spawn ${bin} ${args.join(" ")}`);
      try {
        adapterProc = cp.spawn(bin, args);
      } catch (e) {
        const msg = `cannot spawn goja-dap: ${e.message}`;
        output.appendLine(msg);
        vscode.window.showErrorMessage(msg);
        return undefined;
      }
      adapterProc.stdout.on("data", (d) => output.appendLine("[goja-dap] " + d));
      adapterProc.stderr.on("data", (d) => output.appendLine("[goja-dap!] " + d));
      adapterProc.on("error", (e) => {
        const msg =
          `cannot start goja-dap (${bin}): ${e.message}. ` +
          `Build it with: go build -o cmd/dap/goja-dap ./cmd/dap`;
        output.appendLine(msg);
        vscode.window.showErrorMessage(msg);
      });
      adapterProc.on("exit", (code, signal) => {
        output.appendLine(`goja-dap exited code=${code} signal=${signal}`);
      });

      // give the server a moment to start listening
      return new Promise((resolve, reject) => {
        let tries = 0;
        const tryConnect = () => {
          const sock = net.connect(port, "127.0.0.1");
          sock.once("connect", () => {
            output.appendLine(`connected to goja-dap on 127.0.0.1:${port} (probe)`);
            sock.end();
            resolve(new vscode.DebugAdapterServer(port, "127.0.0.1"));
          });
          sock.once("error", (err) => {
            if (++tries > 50) {
              const msg = `goja-dap did not start listening on 127.0.0.1:${port}: ${err.message}`;
              output.appendLine(msg);
              reject(new Error(msg));
            } else {
              setTimeout(tryConnect, 100);
            }
          });
        };
        tryConnect();
      });
    }

    // attach
    output.appendLine(`attach to 127.0.0.1:${port}`);
    return new vscode.DebugAdapterServer(port, "127.0.0.1");
  }
}

function deactivate() {
  if (adapterProc) {
    adapterProc.kill();
    adapterProc = undefined;
  }
}

module.exports = { activate, deactivate };
