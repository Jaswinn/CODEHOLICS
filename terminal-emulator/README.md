# CodeHolic Terminal

A Windows desktop terminal emulator, separate from the `godb` database program. It uses Wails for the desktop window, xterm.js for terminal rendering, and Windows ConPTY to host PowerShell.

## Requirements

- Windows 10 version 1809 or later
- Go 1.26+
- Node.js 20+ and npm
- Microsoft Edge WebView2 Runtime

## Build and run

From this directory:

```powershell
npm ci
npm run build
go run .
```

Or build the standalone executable:

```powershell
go build -o CodeHolicTerminal.exe .
.\CodeHolicTerminal.exe
```

The window opens with system metrics, PowerShell terminal tabs, and a two-pane split action. Closing a tab closes its PowerShell sessions.

Run the backend tests with:

```powershell
go test ./...
```

The process panel lists up to eight process names and IDs in alphabetical order. CPU, memory, uptime, and process information are read locally and are not sent to a server.
