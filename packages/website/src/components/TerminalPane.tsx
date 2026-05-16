"use client";

import { useEffect, useRef, useState } from "react";
import type { FitAddon } from "@xterm/addon-fit";
import type { IDisposable, Terminal as XTermTerminal } from "@xterm/xterm";
import { RefreshCw, Wifi, WifiOff } from "lucide-react";
import type { SessionInfo } from "@/lib/api";
import { authenticatedWebSocketURL } from "@/lib/api";
import { openAgentSocket } from "@/lib/agentSocket";
import { openWebShellSocket, type WebShellSocket } from "@/lib/secureWebSocket";
import { base64ToBytes, type UnlockedSSHKey } from "@/lib/sshKey";

type TerminalPaneProps = {
  session: SessionInfo | null;
  fontSize: number;
  reconnectToken: number;
  sshKey: UnlockedSSHKey | null;
  onReconnect: () => void;
  onSessionExit: (sessionId: string) => void;
};

export function TerminalPane({
  session,
  fontSize,
  reconnectToken,
  sshKey,
  onReconnect,
  onSessionExit,
}: TerminalPaneProps) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const terminalRef = useRef<XTermTerminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const socketRef = useRef<WebShellSocket | null>(null);
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    if (terminalRef.current) {
      terminalRef.current.options.fontSize = fontSize;
    }
    scheduleFit(socketRef.current, terminalRef.current, fitRef.current);
  }, [fontSize]);

  useEffect(() => {
    if (!session?.id || !containerRef.current) {
      return;
    }
    const sessionId = session.id;

    let disposed = false;
    let resizeObserver: ResizeObserver | null = null;
    let dataDisposable: IDisposable | null = null;
    let resizeTimer: number | null = null;
    let debouncedFit: (() => void) | null = null;
    let terminal: XTermTerminal | null = null;
    let fit: FitAddon | null = null;
    let socket: WebShellSocket | null = null;
    let agentSocket: WebSocket | null = null;
    let replayingHistory = false;
    let sessionExitHandled = false;

    const handleSessionExit = () => {
      if (sessionExitHandled) {
        return;
      }
      sessionExitHandled = true;
      setConnected(false);
      onSessionExit(sessionId);
    };

    async function mountTerminal() {
      const container = containerRef.current;
      if (!container) {
        return;
      }

      const [{ Terminal }, { FitAddon }] = await Promise.all([
        import("@xterm/xterm"),
        import("@xterm/addon-fit"),
      ]);

      if (disposed) {
        return;
      }

      container.innerHTML = "";
      terminal = new Terminal({
        allowTransparency: false,
        convertEol: false,
        cursorBlink: true,
        cursorStyle: "block",
        fontFamily:
          'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace',
        fontSize,
        letterSpacing: 0,
        lineHeight: 1.18,
        macOptionIsMeta: true,
        scrollback: 8000,
        tabStopWidth: 8,
        theme: {
          background: "#0a0a0a",
          foreground: "#e5e5e5",
          cursor: "#fafafa",
          cursorAccent: "#0a0a0a",
          selectionBackground: "#525252",
          black: "#171717",
          red: "#ef4444",
          green: "#22c55e",
          yellow: "#eab308",
          blue: "#60a5fa",
          magenta: "#d946ef",
          cyan: "#06b6d4",
          white: "#e5e5e5",
          brightBlack: "#737373",
          brightRed: "#f87171",
          brightGreen: "#4ade80",
          brightYellow: "#facc15",
          brightBlue: "#93c5fd",
          brightMagenta: "#f0abfc",
          brightCyan: "#67e8f9",
          brightWhite: "#ffffff",
        },
      });

      fit = new FitAddon();
      terminal.loadAddon(fit);
      terminal.open(container);
      tuneMobileTextarea(container);

      terminalRef.current = terminal;
      fitRef.current = fit;

      socket = await openWebShellSocket(authenticatedWebSocketURL(`/api/sessions/${sessionId}/ws`));
      socket.binaryType = "arraybuffer";
      socketRef.current = socket;
      agentSocket = openAgentSocket(sessionId, sshKey);

      const fitAndSend = () => {
        if (!terminal || !fit || disposed) {
          return;
        }
        try {
          fit.fit();
          if (socket?.readyState === WebSocket.OPEN && terminal.cols > 0 && terminal.rows > 0) {
            socket.send(
              JSON.stringify({
                type: "resize",
                cols: terminal.cols,
                rows: terminal.rows,
              }),
            );
          }
        } catch {
          // The fit addon can throw during fast remounts while layout is settling.
        }
      };

      debouncedFit = () => {
        if (resizeTimer !== null) {
          window.clearTimeout(resizeTimer);
        }
        resizeTimer = window.setTimeout(fitAndSend, 80);
      };

      socket.addEventListener("open", () => {
        setConnected(true);
        fitAndSend();
        terminal?.focus();
      });

      socket.addEventListener("close", (event) => {
        setConnected(false);
        if (event.code === 1000 && event.reason === "session exited") {
          handleSessionExit();
        }
      });

      socket.addEventListener("error", () => {
        setConnected(false);
      });

      socket.addEventListener("message", async (event) => {
        if (!terminal) {
          return;
        }
        if (event.data instanceof ArrayBuffer) {
          terminal.write(new Uint8Array(event.data));
          return;
        }
        if (event.data instanceof Blob) {
          terminal.write(new Uint8Array(await event.data.arrayBuffer()));
          return;
        }
        const message = parseServerMessage(String(event.data));
        if (message?.type === "history" && typeof message.data === "string") {
          replayingHistory = true;
          terminal.write(base64ToBytes(message.data), () => {
            replayingHistory = false;
          });
          return;
        }
        if (message?.type === "exit") {
          handleSessionExit();
          socket?.close();
          return;
        }
        terminal.write(String(event.data));
      });

      dataDisposable = terminal.onData((data) => {
        if (!replayingHistory && socket?.readyState === WebSocket.OPEN) {
          socket.send(JSON.stringify({ type: "input", data }));
        }
      });

      resizeObserver = new ResizeObserver(debouncedFit);
      resizeObserver.observe(container);
      window.addEventListener("resize", debouncedFit);
      window.visualViewport?.addEventListener("resize", debouncedFit);
      document.fonts?.ready.then(debouncedFit).catch(() => undefined);
      window.setTimeout(fitAndSend, 0);
    }

    void mountTerminal();

    return () => {
      disposed = true;
      setConnected(false);
      if (resizeTimer !== null) {
        window.clearTimeout(resizeTimer);
      }
      resizeObserver?.disconnect();
      if (debouncedFit) {
        window.removeEventListener("resize", debouncedFit);
        window.visualViewport?.removeEventListener("resize", debouncedFit);
      }
      dataDisposable?.dispose();
      socket?.close();
      agentSocket?.close();
      terminal?.dispose();
      if (socketRef.current === socket) {
        socketRef.current = null;
      }
      if (terminalRef.current === terminal) {
        terminalRef.current = null;
      }
      if (fitRef.current === fit) {
        fitRef.current = null;
      }
    };
  }, [session?.id, fontSize, reconnectToken, sshKey, onSessionExit]);

  if (!session) {
    return (
      <div className="flex h-full items-center justify-center bg-neutral-950 text-sm text-neutral-500">
        No session selected
      </div>
    );
  }

  return (
    <section className="flex h-full min-h-0 flex-col bg-neutral-950">
      <div className="flex h-11 shrink-0 items-center justify-between border-b border-neutral-800 bg-neutral-950 px-3">
        <div className="flex min-w-0 items-center gap-2">
          {connected ? (
            <Wifi className="h-4 w-4 shrink-0 text-emerald-400" aria-hidden="true" />
          ) : (
            <WifiOff className="h-4 w-4 shrink-0 text-amber-400" aria-hidden="true" />
          )}
          <span className="truncate text-sm font-medium text-neutral-100">{session.name}</span>
          <span className="hidden text-xs text-neutral-500 sm:inline">
            {connected ? "connected" : "disconnected"}
          </span>
        </div>

        <button
          type="button"
          onClick={onReconnect}
          title="Reconnect"
          className="grid h-8 w-8 place-items-center rounded-md text-neutral-300 hover:bg-neutral-800 hover:text-neutral-50"
        >
          <RefreshCw className="h-4 w-4" aria-hidden="true" />
          <span className="sr-only">Reconnect</span>
        </button>
      </div>

      <div
        ref={containerRef}
        className="min-h-0 flex-1 overflow-hidden bg-neutral-950"
        onPointerDown={() => terminalRef.current?.focus()}
      />
    </section>
  );
}

function scheduleFit(
  socket: WebShellSocket | null,
  terminal: XTermTerminal | null,
  fit: FitAddon | null,
) {
  window.setTimeout(() => {
    if (!terminal || !fit) {
      return;
    }
    try {
      fit.fit();
      if (socket?.readyState === WebSocket.OPEN && terminal.cols > 0 && terminal.rows > 0) {
        socket.send(JSON.stringify({ type: "resize", cols: terminal.cols, rows: terminal.rows }));
      }
    } catch {
      // Layout may be unavailable while the component is being replaced.
    }
  }, 0);
}

function parseServerMessage(payload: string) {
  try {
    return JSON.parse(payload) as { type?: string; data?: string };
  } catch {
    return null;
  }
}

function tuneMobileTextarea(container: HTMLElement) {
  const textarea = container.querySelector("textarea");
  textarea?.setAttribute("autocapitalize", "none");
  textarea?.setAttribute("autocomplete", "off");
  textarea?.setAttribute("autocorrect", "off");
  textarea?.setAttribute("spellcheck", "false");
}
