# VS Code 调试 goja

这个目录是一个最小 VS Code 调试扩展（纯 JS，不用 npm install / 编译），把 VS Code 的 DAP 客户端接到 `goja-dap` 上。

## 一次性安装（5 分钟）

1. 构建适配器：

   ```sh
   go build -o cmd/dap/goja-dap.exe ./cmd/dap   # Windows
   go build -o cmd/dap/goja-dap ./cmd/dap       # macOS/Linux
   ```

   把 `goja-dap` 放到 PATH 里，或记住它的绝对路径。

2. 安装扩展到 VS Code：运行本目录下的安装脚本（默认软链，改了代码立即生效）

   - Windows（PowerShell）:
     ```powershell
     powershell -ExecutionPolicy Bypass -File install.ps1             # 软链
     powershell -ExecutionPolicy Bypass -File install.ps1 -Copy       # 改为复制
     powershell -ExecutionPolicy Bypass -File install.ps1 -Uninstall  # 卸载
     ```
   - macOS/Linux:
     ```sh
     ./install.sh              # 软链
     ./install.sh --copy       # 改为复制
     ./install.sh --uninstall  # 卸载
     ```

   脚本会把本目录软链/复制到 VS Code 扩展目录（`~/.vscode/extensions/goja-debug`）。Windows 上软链需要管理员权限或开启开发者模式，失败时脚本会自动回退为复制。

3. 重启 VS Code。

## 使用

### Launch（推荐，一步到位）

打开任意 `.js` 文件，在 VS Code 左侧"运行和调试"里创建 `launch.json`：

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "type": "goja",
      "request": "launch",
      "name": "Debug current JS file (goja)",
      "program": "${file}",
      "port": 5678,
      "gojaDapPath": "E:/goworkmod/liumingmin/goja/cmd/dap/goja-dap.exe"
    }
  ]
}
```

然后打开要调试的 JS 文件 → 打断点 → F5。

### Attach（脚本已在跑）

先手动起服务：

```sh
goja-dap -port 5678 path/to/script.js
```

`launch.json`：

```json
{
  "type": "goja",
  "request": "attach",
  "name": "Attach to goja-dap",
  "port": 5678
}
```

F5 连接。

## 功能

- 断点、条件断点（暂不支持条件）、step over/into/out
- 变量窗格：Local / Global 作用域，对象可展开
- 调试控制台（DEBUG CONSOLE）直接输入 JS 表达式，在暂停帧上下文里求值
- `console.log` 出现在 DEBUG CONSOLE
- `debugger;` 语句触发暂停

## 限制

- 一次会话一个脚本；launch 模式下脚本每次 F5 从头跑
- 断点按「文件基名 + 行号」匹配
- `pause`（运行中点暂停按钮）暂不支持
