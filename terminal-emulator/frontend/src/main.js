import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import "./style.css";

const app = document.querySelector("#app");
const terminals = new Map();
const pendingOutput = new Map();
let tabs = [];
let activeTab = "";
let systemEventsReady = false;
let nextTabNumber = 1;

const escapeHTML = (value) => String(value).replace(/[&<>"']/g, (char) => ({
  "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
})[char]);

function active() {
  return tabs.find((tab) => tab.id === activeTab);
}

function render() {
  const current = active();
  for (const { terminal } of terminals.values()) {
    terminal.element?.remove();
  }
  app.innerHTML = `
    <div class="shell">
      <aside class="rail">
        <div class="brand-mark">CH</div>
        <div class="rail-label">MACHINE</div>
        <section class="metric-card">
          <div class="eyebrow">CPU LOAD</div>
          <div class="metric-value" id="cpu-value">--<small>%</small></div>
          <div class="meter"><i id="cpu-meter"></i></div>
          <div class="metric-note">PROCESSOR</div>
        </section>
        <section class="metric-card">
          <div class="eyebrow">MEMORY</div>
          <div class="metric-value" id="memory-value">--<small>%</small></div>
          <div class="meter"><i id="memory-meter"></i></div>
          <div class="metric-note" id="memory-note">-- / -- GB</div>
        </section>
        <section class="metric-card uptime-card">
          <div class="eyebrow">UPTIME</div>
          <div class="uptime-value" id="uptime-value">--</div>
        </section>
        <section class="process-card">
          <div class="eyebrow">PROCESSES</div>
          <div id="process-list" class="process-list">Waiting for system stats…</div>
        </section>
        <div class="rail-footer"><span class="live-dot"></span> WINDOWS SESSION</div>
      </aside>
      <main class="workspace">
        <header class="topbar">
          <div class="window-dots"><i></i><i></i><i></i></div>
          <div class="window-title">CODEHOLIC <span>/</span> TERMINAL</div>
          <div class="topbar-status"><span class="live-dot"></span> LOCAL</div>
        </header>
        <nav class="tabs" aria-label="Terminal tabs">
          ${tabs.map((tab) => `<button class="tab ${tab.id === activeTab ? "selected" : ""}" data-tab="${tab.id}">
            <span class="tab-light"></span>${escapeHTML(tab.name)}<span class="tab-close" data-close="${tab.id}" aria-label="Close tab">×</span>
          </button>`).join("")}
          <button class="tab-add" id="new-tab" title="New terminal tab">+</button>
          <div class="tab-actions">
            <button class="tool-button" id="split-terminal" title="Split terminal">SPLIT +</button>
          </div>
        </nav>
        <section class="terminal-stage">
          ${current ? current.panes.map((pane, index) => `<div class="terminal-pane ${current.panes.length > 1 ? "split-pane" : ""}" data-pane="${pane}">
            <div class="pane-title"><span>POWERSHELL</span><span>SESSION ${String(index + 1).padStart(2, "0")}</span></div>
            <div class="terminal-mount" id="term-${pane}"></div>
          </div>`).join("") : `<div class="empty-state"><div class="empty-mark">CH</div><h1>NO ACTIVE SESSION</h1><p>Open a terminal tab to start PowerShell.</p><button class="primary-button" id="empty-new">NEW SESSION</button></div>`}
        </section>
        <footer class="statusbar"><span><i class="live-dot"></i> ${current ? "CONPTY CONNECTED" : "NO ACTIVE SESSION"}</span><span id="cwd-status">LOCAL POWERSHELL</span><span>UTF-8 <b>●</b></span></footer>
      </main>
    </div>`;
  bindUI();
  if (current) {
    for (const pane of current.panes) {
      const instance = terminals.get(pane);
      const mount = document.querySelector(`#term-${CSS.escape(pane)}`);
      if (instance && mount) {
        mount.append(instance.terminal.element);
        instance.fit.fit();
      } else {
        mountTerminal(pane);
      }
    }
  }
  bindSystemEvents();
}

function bindUI() {
  document.querySelector("#new-tab")?.addEventListener("click", createTab);
  document.querySelector("#empty-new")?.addEventListener("click", createTab);
  document.querySelector("#split-terminal")?.addEventListener("click", splitPane);
  document.querySelectorAll("[data-tab]").forEach((button) => {
    button.addEventListener("click", () => {
      activeTab = button.dataset.tab;
      render();
    });
  });
  document.querySelectorAll("[data-close]").forEach((button) => {
    button.addEventListener("click", (event) => {
      event.stopPropagation();
      closeTab(button.dataset.close);
    });
  });
}

function mountTerminal(id) {
  const mount = document.querySelector(`#term-${CSS.escape(id)}`);
  if (!mount || terminals.has(id)) return;
  const terminal = new Terminal({
    allowProposedApi: false,
    cursorBlink: true,
    fontFamily: '"Cascadia Mono", "Consolas", monospace',
    fontSize: 13,
    lineHeight: 1.25,
    scrollback: 5000,
    theme: {
      background: "#090b0b",
      foreground: "#d8d6ca",
      cursor: "#ff793d",
      cursorAccent: "#090b0b",
      selectionBackground: "#73361f",
      black: "#151918",
      red: "#e56d56",
      green: "#a7bd70",
      yellow: "#e6b45e",
      blue: "#75a6b8",
      magenta: "#bd83aa",
      cyan: "#6ac9c6",
      white: "#dedbd0",
      brightBlack: "#777970",
      brightRed: "#ff8c73",
      brightGreen: "#c5d98c",
      brightYellow: "#ffd17c",
      brightBlue: "#91c4d7",
      brightMagenta: "#dfa1c8",
      brightCyan: "#84e5df",
      brightWhite: "#fffdf1"
    }
  });
  const fit = new FitAddon();
  terminal.loadAddon(fit);
  terminal.open(mount);
  fit.fit();
  terminal.onData((data) => window.go.main.App.SendInput(id, data).catch(showError));
  terminal.onResize(({ cols, rows }) => window.go.main.App.ResizeTerminal(id, cols, rows).catch(showError));
  terminals.set(id, { terminal, fit });
  const pending = pendingOutput.get(id);
  if (pending) {
    terminal.write(pending);
    pendingOutput.delete(id);
  }
  requestAnimationFrame(() => {
    fit.fit();
    window.go.main.App.ResizeTerminal(id, terminal.cols, terminal.rows).catch(showError);
    terminal.focus();
  });
}

async function newSession() {
  const mount = document.createElement("div");
  mount.className = "terminal-mount staging-mount";
  document.body.append(mount);
  const rect = document.querySelector(".terminal-stage")?.getBoundingClientRect();
  const width = Math.max(20, Math.floor((rect?.width ?? 900) / 8));
  const height = Math.max(5, Math.floor((rect?.height ?? 600) / 18));
  mount.remove();
  const id = await window.go.main.App.StartTerminal(width, height);
  return id;
}

async function createTab() {
  try {
    const id = await newSession();
    const tab = { id: `tab-${id}`, name: `SHELL ${String(nextTabNumber++).padStart(2, "0")}`, panes: [id] };
    tabs.push(tab);
    activeTab = tab.id;
    render();
  } catch (error) {
    showError(error);
  }
}

async function splitPane() {
  const tab = active();
  if (!tab) return createTab();
  if (tab.panes.length >= 2) {
    showToast("This MVP supports two split panes per tab.");
    return;
  }
  try {
    tab.panes.push(await newSession());
    render();
  } catch (error) {
    showError(error);
  }
}

async function closeTab(id) {
  const index = tabs.findIndex((tab) => tab.id === id);
  if (index < 0) return;
  const [tab] = tabs.splice(index, 1);
  for (const pane of tab.panes) {
    const instance = terminals.get(pane);
    instance?.terminal.dispose();
    terminals.delete(pane);
    await window.go.main.App.CloseTerminal(pane).catch(showError);
  }
  if (activeTab === id) activeTab = tabs[Math.min(index, tabs.length - 1)]?.id ?? "";
  render();
}

function bindSystemEvents() {
  if (systemEventsReady || !window.runtime?.EventsOn) return;
  systemEventsReady = true;
  window.runtime.EventsOn("terminal:output", (event) => {
    const instance = terminals.get(event.id);
    if (instance) {
      instance.terminal.write(event.data);
      return;
    }
    const buffered = pendingOutput.get(event.id) ?? "";
    pendingOutput.set(event.id, (buffered + event.data).slice(-1_000_000));
  });
  window.runtime.EventsOn("terminal:closed", (event) => {
    terminals.get(event.id)?.terminal.write(`\r\n\x1b[90m[process exited: ${event.code}]\x1b[0m\r\n`);
  });
  window.runtime.EventsOn("system:stats", updateStats);
  window.runtime.EventsOn("app:error", showError);
  window.go.main.App.StartSystemMonitor().catch(showError);
}

function updateStats(stats) {
  const cpu = Math.min(100, Math.max(0, stats.cpu));
  const memory = Math.min(100, Math.max(0, stats.memoryPercent));
  const cpuValue = document.querySelector("#cpu-value");
  if (!cpuValue) return;
  cpuValue.innerHTML = `${cpu.toFixed(0)}<small>%</small>`;
  document.querySelector("#cpu-meter").style.width = `${cpu}%`;
  document.querySelector("#memory-value").innerHTML = `${memory.toFixed(0)}<small>%</small>`;
  document.querySelector("#memory-meter").style.width = `${memory}%`;
  document.querySelector("#memory-note").textContent = `${stats.memoryUsedGB.toFixed(1)} / ${stats.memoryTotalGB.toFixed(1)} GB`;
  document.querySelector("#uptime-value").textContent = formatUptime(stats.uptimeSeconds);
  document.querySelector("#process-list").innerHTML = stats.processes.map((process) =>
    `<div class="process-row"><span>${escapeHTML(process.name)}</span><b>${process.pid}</b></div>`
  ).join("") || "No process data.";
}

function formatUptime(seconds) {
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  return `${days}d ${String(hours).padStart(2, "0")}h ${String(minutes).padStart(2, "0")}m`;
}

function showError(error) {
  showToast(error?.message ?? String(error));
}

let toastTimer;
function showToast(message) {
  let toast = document.querySelector(".toast");
  if (!toast) {
    toast = document.createElement("div");
    toast.className = "toast";
    document.body.append(toast);
  }
  toast.textContent = message;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toast.remove(), 4500);
}

window.addEventListener("resize", () => {
  const current = active();
  for (const id of current?.panes ?? []) {
    const { terminal, fit } = terminals.get(id) ?? {};
    if (!terminal || !fit) continue;
    fit.fit();
    window.go.main.App.ResizeTerminal(id, terminal.cols, terminal.rows).catch(showError);
  }
});

render();
