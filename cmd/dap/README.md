# goja DAP 调试器

`goja-dap` 是一个 [Debug Adapter Protocol](https://microsoft.github.io/debug-adapter-protocol/) 服务器，基于本仓库的 VM 级调试器（`debugger.go` + `vm.debug()`），让你可以在 VS Code（或任何 DAP 客户端）里调试跑在 goja 里的 JavaScript。

## 功能

- 断点（`setBreakpoints`）
- 单步：step over / step in / step out（`next` / `stepIn` / `stepOut`）
- 调用栈（`stackTrace`）
- 变量检查：Local / Global 作用域，对象可懒展开（`scopes` / `variables`）
- 调试控制台表达式求值（`evaluate`，在暂停帧的上下文里执行）
- `console.log` 输出转发为 DAP `output` 事件
- `debugger;` 语句触发暂停

## 构建

```sh
go build -o goja-dap ./cmd/dap
```

## 使用

### TCP 模式（attach）

```sh
./goja-dap -port 5678 path/to/script.js
```

VS Code `launch.json`（配合任意 DAP 客户端扩展，或 "Debugger for goja"）：

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "type": "goja",
      "request": "attach",
      "name": "Attach to goja",
      "debugServer": 5678
    }
  ]
}
```

### stdio 模式（编辑器托管 launch）

```sh
./goja-dap -stdio path/to/script.js
```

## 协议时序

```
client                       goja-dap                       goja VM (debug loop)
  |-- initialize ------------>|                                 |
  |<-- InitializeResponse + InitializedEvent                    |
  |-- attach ---------------->|                                 |
  |<-- AttachResponse         |                                 |
  |-- setBreakpoints -------->|  SetBreakpoint()                |
  |-- configurationDone ----->|  start(): go vm.RunScript() ---->| vm.debug()
  |                           |  pump: dbg.Continue() --------->| ...hit breakpoint...
  |<-- stopped(breakpoint) ---|<-------------------------------| activate()
  |-- stackTrace / scopes / variables / evaluate -------------->|
  |-- continue / next / stepIn / stepOut ---------------------->| resume
  |                           |  ...                            |
  |<-- terminated ------------|<-------------------------------| program end
```

## 实现结构

| 文件 | 作用 |
|---|---|
| `main.go` | 入口：`-port` / `-stdio` / `-stopOnEntry`，DAP 读写循环 |
| `adapter.go` | 请求分发、断点/栈/作用域/变量/求值、console 转发、writer 队列 |
| `adapter_test.go` | 进程内管道冒烟测试（`go test ./...`） |
| `testdata/sample.js` | 冒烟测试脚本 |

VM 级调试器在本仓库根目录 `debugger.go`（`Runtime.AttachDebugger()` 返回 `*goja.Debugger`），相关钩子在 `vm.go`（`vm.debug()` 循环、`_debugger` 指令）、`compiler.go`（debug 模式禁用动态作用域优化）、`compiler_stmt.go`（debug 模式下发射 `debugger` 指令）、`runtime.go`（`compileDebug`）。

## 限制

- 一次会话只调试一个脚本；每次 TCP 连接重新从头跑一遍脚本。
- `pause`（异步中断）暂不支持。
- 断点按「文件基名 + 行号」匹配，多文件同名时可能互相干扰。
