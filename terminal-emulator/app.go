package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/UserExistsError/conpty"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx       context.Context
	mu        sync.RWMutex
	sessions  map[string]*terminalSession
	nextID    atomic.Uint64
	statsOnce sync.Once
}

type terminalSession struct {
	console *conpty.ConPty
	mu      sync.Mutex
	closed  bool
}

type processInfo struct {
	Name string `json:"name"`
	PID  int32  `json:"pid"`
}

type systemStats struct {
	CPU           float64       `json:"cpu"`
	MemoryPercent float64       `json:"memoryPercent"`
	MemoryUsedGB  float64       `json:"memoryUsedGB"`
	MemoryTotalGB float64       `json:"memoryTotalGB"`
	UptimeSeconds uint64        `json:"uptimeSeconds"`
	Processes     []processInfo `json:"processes"`
}

func NewApp() *App {
	return &App{sessions: make(map[string]*terminalSession)}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) shutdown(context.Context) {
	a.mu.Lock()
	sessions := make([]*terminalSession, 0, len(a.sessions))
	for _, session := range a.sessions {
		sessions = append(sessions, session)
	}
	a.sessions = make(map[string]*terminalSession)
	a.mu.Unlock()

	for _, session := range sessions {
		if err := session.close(); err != nil {
			log.Printf("close terminal: %v", err)
		}
	}
}

func (a *App) StartTerminal(columns, rows int) (string, error) {
	if columns < 20 || columns > 300 || rows < 5 || rows > 200 {
		return "", errors.New("terminal dimensions are out of range")
	}
	if a.ctx == nil {
		return "", errors.New("application is not ready")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	console, err := conpty.Start(
		"powershell.exe -NoLogo -NoExit",
		conpty.ConPtyDimensions(columns, rows),
		conpty.ConPtyWorkDir(home),
	)
	if err != nil {
		return "", fmt.Errorf("start PowerShell: %w", err)
	}
	id := fmt.Sprintf("terminal-%d", a.nextID.Add(1))
	session := &terminalSession{console: console}

	a.mu.Lock()
	a.sessions[id] = session
	a.mu.Unlock()

	go a.readTerminal(id, session)
	go a.waitTerminal(id, session)
	return id, nil
}

func (a *App) SendInput(id, input string) error {
	session, err := a.getSession(id)
	if err != nil {
		return err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return errors.New("terminal session is closed")
	}
	if _, err := session.console.Write([]byte(input)); err != nil {
		return fmt.Errorf("write to terminal: %w", err)
	}
	return nil
}

func (a *App) ResizeTerminal(id string, columns, rows int) error {
	if columns < 20 || columns > 300 || rows < 5 || rows > 200 {
		return errors.New("terminal dimensions are out of range")
	}
	session, err := a.getSession(id)
	if err != nil {
		return err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return errors.New("terminal session is closed")
	}
	if err := session.console.Resize(columns, rows); err != nil {
		return fmt.Errorf("resize terminal: %w", err)
	}
	return nil
}

func (a *App) CloseTerminal(id string) error {
	a.mu.Lock()
	session, ok := a.sessions[id]
	if ok {
		delete(a.sessions, id)
	}
	a.mu.Unlock()
	if !ok {
		return nil
	}
	if err := session.close(); err != nil {
		return fmt.Errorf("close terminal: %w", err)
	}
	return nil
}

func (a *App) StartSystemMonitor() error {
	if a.ctx == nil {
		return errors.New("application is not ready")
	}
	a.statsOnce.Do(func() {
		go a.monitorSystem(a.ctx)
	})
	return nil
}

func (a *App) getSession(id string) (*terminalSession, error) {
	a.mu.RLock()
	session, ok := a.sessions[id]
	a.mu.RUnlock()
	if !ok {
		return nil, errors.New("terminal session not found")
	}
	return session, nil
}

func (s *terminalSession) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.console.Close(); err != nil {
		return err
	}
	return nil
}

func (a *App) readTerminal(id string, session *terminalSession) {
	buffer := make([]byte, 8192)
	for {
		n, err := session.console.Read(buffer)
		if n > 0 {
			runtime.EventsEmit(a.ctx, "terminal:output", map[string]string{
				"id": id, "data": string(buffer[:n]),
			})
		}
		if err != nil {
			return
		}
	}
}

func (a *App) waitTerminal(id string, session *terminalSession) {
	exitCode, err := session.console.Wait(a.ctx)
	if err != nil {
		return
	}
	runtime.EventsEmit(a.ctx, "terminal:closed", map[string]any{
		"id": id, "code": exitCode,
	})
}

func (a *App) monitorSystem(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		stats, err := collectSystemStats(ctx)
		if err != nil {
			runtime.EventsEmit(ctx, "app:error", err.Error())
		} else {
			runtime.EventsEmit(ctx, "system:stats", stats)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func collectSystemStats(ctx context.Context) (systemStats, error) {
	loads, err := cpu.PercentWithContext(ctx, 0, false)
	if err != nil {
		return systemStats{}, fmt.Errorf("read CPU usage: %w", err)
	}
	memory, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return systemStats{}, fmt.Errorf("read memory usage: %w", err)
	}
	uptime, err := host.UptimeWithContext(ctx)
	if err != nil {
		return systemStats{}, fmt.Errorf("read system uptime: %w", err)
	}
	processes, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return systemStats{}, fmt.Errorf("list processes: %w", err)
	}

	top := make([]processInfo, 0, 8)
	for _, item := range processes {
		name, nameErr := item.NameWithContext(ctx)
		if nameErr == nil && name != "" {
			top = append(top, processInfo{Name: name, PID: item.Pid})
		}
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].Name == top[j].Name {
			return top[i].PID < top[j].PID
		}
		return top[i].Name < top[j].Name
	})
	if len(top) > 8 {
		top = top[:8]
	}

	var cpuUsage float64
	if len(loads) > 0 {
		cpuUsage = loads[0]
	}
	return systemStats{
		CPU:           cpuUsage,
		MemoryPercent: memory.UsedPercent,
		MemoryUsedGB:  float64(memory.Used) / (1024 * 1024 * 1024),
		MemoryTotalGB: float64(memory.Total) / (1024 * 1024 * 1024),
		UptimeSeconds: uptime,
		Processes:     top,
	}, nil
}
