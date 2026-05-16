"use client";

import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type DragEvent,
  type KeyboardEvent,
  type PointerEvent,
  type WheelEvent,
} from "react";
import { RefreshCw, Wifi, WifiOff } from "lucide-react";
import type { SessionInfo } from "@/lib/api";
import { api, authenticatedWebSocketURL } from "@/lib/api";
import { openAgentSocket } from "@/lib/agentSocket";
import { createGUIAudioPlayer, resumeGUIAudio, type GUIAudioPlayer } from "@/lib/guiAudio";
import type { UnlockedSSHKey } from "@/lib/sshKey";

type GUIViewerProps = {
  session: SessionInfo | null;
  reconnectToken: number;
  sshKey: UnlockedSSHKey | null;
  onReconnect: () => void;
  onSessionExit: (sessionId: string) => void;
  onError: (message: string) => void;
};

type GUIServerMessage =
  | {
      type: "frame";
      encoding: "jpeg" | "png" | "raw";
      width: number;
      height: number;
      data: string;
    }
  | {
      type: "status";
      state: "starting" | "running" | "exited";
      message?: string;
    };

type GUIBinaryFrame = {
  kind: "full" | "rect" | "audio";
  screenWidth: number;
  screenHeight: number;
  x: number;
  y: number;
  width: number;
  height: number;
  data: ArrayBuffer;
};

type GUITransport = {
  readyState: number;
  send(data: string): void;
  close(): void;
};

const wheelFlushMs = 32;
const wheelLinePixels = 40;
const wheelPagePixels = 600;
const pointerMoveFlushMs = 16;
const audioMaxQueueChunks = 96;
const binaryAudioFrameHeaderLen = 12;

export function GUIViewer({
  session,
  reconnectToken,
  sshKey,
  onReconnect,
  onSessionExit,
  onError,
}: GUIViewerProps) {
  const shellRef = useRef<HTMLDivElement | null>(null);
  const socketRef = useRef<GUITransport | null>(null);
  const audioRef = useRef<GUIAudioPlayer | null>(null);
  const videoRendererRef = useRef<GUIVideoRenderer | null>(null);
  const remoteSizeRef = useRef({ width: 1, height: 1 });
  const forwardedKeysRef = useRef<Set<string>>(new Set());
  const pointerMoveRef = useRef({
    pending: false,
    timer: null as number | null,
    x: 0,
    y: 0,
  });
  const wheelRef = useRef({
    deltaX: 0,
    deltaY: 0,
    timer: null as number | null,
    x: 0,
    y: 0,
  });
  const dragDepthRef = useRef(0);
  const [connected, setConnected] = useState(false);
  const [status, setStatus] = useState("disconnected");
  const [focused, setFocused] = useState(false);
  const [dragActive, setDragActive] = useState(false);
  const [dropBusy, setDropBusy] = useState(false);

  const setVideoRenderer = useCallback((renderer: GUIVideoRenderer | null) => {
    videoRendererRef.current = renderer;
  }, []);

  useEffect(() => {
    if (!session?.id || !shellRef.current) {
      return;
    }

    const sessionId = session.id;
    const shell = shellRef.current;
    let disposed = false;
    let resizeObserver: ResizeObserver | null = null;
    let resizeTimer: number | null = null;
    let connectTimer: number | null = null;
    let socket: GUITransport | null = null;
    let transportWorker: Worker | null = null;
    let agentSocket: WebSocket | null = null;
    let sessionExitHandled = false;
    let binaryFrameQueue = Promise.resolve();
    let audio: GUIAudioPlayer | null = null;
    const audioQueue: Array<{ data: ArrayBuffer; sampleRate: number; channels: number }> = [];
    remoteSizeRef.current = {
      width: Math.max(1, session.width || 1),
      height: Math.max(1, session.height || 1),
    };
    wheelRef.current.deltaX = 0;
    wheelRef.current.deltaY = 0;
    wheelRef.current.timer = null;
    pointerMoveRef.current.pending = false;
    pointerMoveRef.current.timer = null;

    const resetForwardedKeys = () => {
      forwardedKeysRef.current.clear();
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: "keyReset" }));
      }
    };

    const handleSessionExit = () => {
      if (sessionExitHandled) {
        return;
      }
      sessionExitHandled = true;
      setConnected(false);
      setStatus("exited");
      onSessionExit(sessionId);
    };

    const drawFrame = (message: Extract<GUIServerMessage, { type: "frame" }>) => {
      remoteSizeRef.current = {
        width: Math.max(1, message.width),
        height: Math.max(1, message.height),
      };
      videoRendererRef.current?.renderLegacyFrame(message);
    };

    const drawBinaryFrame = (buffer: ArrayBuffer) => {
      const dimensions = binaryFrameDimensions(buffer);
      if (dimensions) {
        remoteSizeRef.current = dimensions;
      }
      videoRendererRef.current?.renderBinaryFrame(buffer);
      setStatus("running");
    };

    const setupAudio = async () => {
      audio = await createGUIAudioPlayer();
      if (disposed) {
        audio.close();
        audio = null;
        return;
      }
      audioRef.current = audio;
      flushAudioQueue();
    };

    const enqueueAudio = (data: ArrayBuffer, sampleRate: number, channels: number) => {
      if (!audio) {
        audioQueue.push({ data, sampleRate, channels });
        if (audioQueue.length > audioMaxQueueChunks) {
          audioQueue.splice(0, audioQueue.length - audioMaxQueueChunks);
        }
        return;
      }
      audioQueue.push({ data, sampleRate, channels });
      if (audioQueue.length > audioMaxQueueChunks) {
        audioQueue.splice(0, audioQueue.length - audioMaxQueueChunks);
      }
      flushAudioQueue();
    };

    const flushAudioQueue = () => {
      if (!audio) {
        return;
      }
      while (audioQueue.length > 0) {
        const chunk = audioQueue.shift()!;
        audio.pushPCM(chunk.data, chunk.sampleRate, chunk.channels);
      }
      audio.resume();
    };

    const handleBinaryData = (data: ArrayBuffer | Blob) => {
      void (async () => {
        const buffer = data instanceof Blob ? await data.arrayBuffer() : data;
        const audioFrame = parseBinaryAudioFrame(buffer);
        if (audioFrame) {
          enqueueAudio(audioFrame.data, audioFrame.sampleRate, audioFrame.channels);
          return;
        }
        binaryFrameQueue = binaryFrameQueue.then(() => drawBinaryFrame(buffer));
      })().catch(() => {
        setStatus("frame decode failed");
      });
    };

    const sendResize = () => {
      if (!socket || socket.readyState !== WebSocket.OPEN || disposed) {
        return;
      }
      const rect = shell.getBoundingClientRect();
      const width = Math.max(1, Math.floor(rect.width));
      const height = Math.max(1, Math.floor(rect.height));
      socket.send(JSON.stringify({ type: "resize", width, height }));
    };

    const scheduleResize = () => {
      if (resizeTimer !== null) {
        window.clearTimeout(resizeTimer);
      }
      resizeTimer = window.setTimeout(sendResize, 300);
    };

    const startConnection = () => {
      if (disposed) {
        return;
      }
      connectTimer = null;
      setStatus("connecting");
      try {
        transportWorker = new Worker(new URL("../workers/guiTransportWorker.ts", import.meta.url), {
          type: "module",
        });
      } catch (err) {
        setStatus(err instanceof Error ? err.message : "connection worker unavailable");
        return;
      }
      socket = workerTransport(transportWorker);
      socketRef.current = socket;
      agentSocket = openAgentSocket(sessionId, sshKey);
      transportWorker.onmessage = (event) => {
        if (disposed) {
          return;
        }
        const workerMessage = event.data;
        if (workerMessage.type === "open") {
          socket!.readyState = WebSocket.OPEN;
          setConnected(true);
          setStatus("connected");
          shell.focus();
          sendResize();
          return;
        }
        if (workerMessage.type === "close") {
          if (socket) {
            socket.readyState = WebSocket.CLOSED;
          }
          setConnected(false);
          if (workerMessage.code === 1000 && workerMessage.reason === "gui session exited") {
            handleSessionExit();
            return;
          }
          setStatus(workerMessage.reason ? `disconnected: ${workerMessage.reason}` : "disconnected");
          return;
        }
        if (workerMessage.type === "error") {
          setConnected(false);
          setStatus(workerMessage.message ?? "connection error");
          return;
        }
        if (workerMessage.type === "audio" || workerMessage.type === "video" || workerMessage.type === "binary") {
          handleBinaryData(workerMessage.data);
          return;
        }
        if (workerMessage.type === "message") {
          const message = parseServerMessage(workerMessage.data);
          if (!message) {
            return;
          }
          if (message.type === "status") {
            setStatus(message.message ?? message.state);
            if (message.state === "exited") {
              handleSessionExit();
            }
            return;
          }
          if (message.type === "frame") {
            setStatus("running");
            drawFrame(message);
          }
        }
      };
      transportWorker.postMessage({
        type: "connect",
        url: authenticatedWebSocketURL(`/api/sessions/${sessionId}/gui`),
      });
    };

    try {
      void setupAudio().catch((err) => {
        setStatus(err instanceof Error ? err.message : "audio unavailable");
      });
      resizeObserver = new ResizeObserver(scheduleResize);
      resizeObserver.observe(shell);
      window.addEventListener("resize", scheduleResize);
      window.visualViewport?.addEventListener("resize", scheduleResize);
      window.addEventListener("blur", resetForwardedKeys);
      document.addEventListener("visibilitychange", resetForwardedKeys);
      // React can mount/cleanup/remount effects immediately in development and
      // during fast client transitions. Creating a WebSocket before that cleanup
      // settles causes the browser to send an outgoing close frame before the
      // E2EE clientHello. Deferring actual socket construction makes that
      // cleanup cancellable instead of creating a doomed connection.
      connectTimer = window.setTimeout(startConnection, 50);
      window.setTimeout(sendResize, 0);
    } catch {
      if (!disposed) {
        setStatus("connection error");
      }
    }

    return () => {
      disposed = true;
      setConnected(false);
      forwardedKeysRef.current.clear();
      if (connectTimer !== null) {
        window.clearTimeout(connectTimer);
        connectTimer = null;
      }
      if (resizeTimer !== null) {
        window.clearTimeout(resizeTimer);
      }
      if (wheelRef.current.timer !== null) {
        window.clearTimeout(wheelRef.current.timer);
        wheelRef.current.timer = null;
      }
      if (pointerMoveRef.current.timer !== null) {
        window.clearTimeout(pointerMoveRef.current.timer);
        pointerMoveRef.current.timer = null;
      }
      pointerMoveRef.current.pending = false;
      pointerMoveRef.current.x = 0;
      pointerMoveRef.current.y = 0;
      wheelRef.current.deltaX = 0;
      wheelRef.current.deltaY = 0;
      resizeObserver?.disconnect();
      window.removeEventListener("resize", scheduleResize);
      window.visualViewport?.removeEventListener("resize", scheduleResize);
      window.removeEventListener("blur", resetForwardedKeys);
      document.removeEventListener("visibilitychange", resetForwardedKeys);
      resetForwardedKeys();
      socket?.close();
      transportWorker?.terminate();
      agentSocket?.close();
      audio?.close();
      if (audioRef.current === audio) {
        audioRef.current = null;
      }
      if (socketRef.current === socket) {
        socketRef.current = null;
      }
    };
  }, [session?.id, reconnectToken, sshKey, onSessionExit]);

  if (!session) {
    return (
      <div className="flex h-full items-center justify-center bg-neutral-950 text-sm text-neutral-500">
        No session selected
      </div>
    );
  }

  const flushPointerMove = () => {
    const socket = socketRef.current;
    const pointerMove = pointerMoveRef.current;
    if (pointerMove.timer !== null) {
      window.clearTimeout(pointerMove.timer);
      pointerMove.timer = null;
    }
    if (!pointerMove.pending || !socket || socket.readyState !== WebSocket.OPEN) {
      return;
    }
    pointerMove.pending = false;
    socket.send(
      JSON.stringify({
        type: "mouse",
        x: pointerMove.x,
        y: pointerMove.y,
        button: 0,
        down: false,
      }),
    );
  };

  const sendPointer = (event: PointerEvent<HTMLDivElement>, down?: boolean) => {
    const socket = socketRef.current;
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      return;
    }
    const { x, y } = pointerToRemote(event, remoteSizeRef.current);
    if (down === undefined) {
      const pointerMove = pointerMoveRef.current;
      pointerMove.x = x;
      pointerMove.y = y;
      pointerMove.pending = true;
      if (pointerMove.timer !== null) {
        return;
      }
      pointerMove.timer = window.setTimeout(() => {
        pointerMove.timer = null;
        const socket = socketRef.current;
        if (!pointerMove.pending || socket?.readyState !== WebSocket.OPEN) {
          return;
        }
        pointerMove.pending = false;
        socket.send(
          JSON.stringify({
            type: "mouse",
            x: pointerMove.x,
            y: pointerMove.y,
            button: 0,
            down: false,
          }),
        );
      }, pointerMoveFlushMs);
      return;
    }

    const pointerMove = pointerMoveRef.current;
    if (pointerMove.timer !== null) {
      window.clearTimeout(pointerMove.timer);
      pointerMove.timer = null;
    }
    pointerMove.pending = false;
    socket.send(
      JSON.stringify({
        type: "mouse",
        x,
        y,
        button: down === undefined ? 0 : pointerButton(event.button),
        down: Boolean(down),
      }),
    );
  };

  const sendWheel = (event: WheelEvent<HTMLDivElement>) => {
    event.preventDefault();
    const socket = socketRef.current;
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      return;
    }
    const { x, y } = pointerToRemote(event, remoteSizeRef.current);
    const wheel = wheelRef.current;
    const factor = wheelDeltaFactor(event.deltaMode);
    wheel.x = x;
    wheel.y = y;
    wheel.deltaX += event.deltaX * factor;
    wheel.deltaY += event.deltaY * factor;
    flushPointerMove();
    if (wheel.timer !== null) {
      return;
    }
    wheel.timer = window.setTimeout(() => {
      wheel.timer = null;
      const deltaX = wheel.deltaX;
      const deltaY = wheel.deltaY;
      wheel.deltaX = 0;
      wheel.deltaY = 0;
      if (Math.abs(deltaX) < 0.5 && Math.abs(deltaY) < 0.5) {
        return;
      }
      if (socketRef.current?.readyState !== WebSocket.OPEN) {
        return;
      }
      socketRef.current.send(
        JSON.stringify({
          type: "wheel",
          x: wheel.x,
          y: wheel.y,
          deltaX,
          deltaY,
        }),
      );
    }, wheelFlushMs);
  };

  const sendKeyReset = () => {
    const socket = socketRef.current;
    forwardedKeysRef.current.clear();
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      return;
    }
    socket.send(JSON.stringify({ type: "keyReset" }));
  };

  const sendKey = (event: KeyboardEvent<HTMLDivElement>, down: boolean) => {
    const keyId = keyStateID(event);
    const alreadyForwarded = forwardedKeysRef.current.has(keyId);
    const shouldForward = shouldForwardKeyEvent(event, down, alreadyForwarded);

    event.preventDefault();
    event.stopPropagation();

    if (!shouldForward) {
      return;
    }
    const socket = socketRef.current;
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      return;
    }

    if (down) {
      forwardedKeysRef.current.add(keyId);
    } else {
      forwardedKeysRef.current.delete(keyId);
    }

    socket.send(
      JSON.stringify({
        type: "key",
        eventType: down ? "keydown" : "keyup",
        key: event.key,
        code: event.code,
        location: event.location,
        repeat: event.repeat,
        shiftKey: event.shiftKey,
        ctrlKey: event.ctrlKey,
        altKey: event.altKey,
        metaKey: event.metaKey,
      }),
    );
  };

  const uploadDroppedFiles = async (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    dragDepthRef.current = 0;
    setDragActive(false);
    if (!session?.id || dropBusy) {
      return;
    }

    const files = await filesFromDrop(event.dataTransfer);
    if (files.length === 0) {
      return;
    }

    setDropBusy(true);
    setStatus("uploading files");
    try {
      const formData = new FormData();
      for (const item of files) {
        formData.append("files", item.file, item.file.name);
        formData.append("paths", item.path);
      }
      await api<{ ok: boolean }>(`/api/sessions/${session.id}/clipboard/files`, {
        method: "PUT",
        body: formData,
      });
      setStatus("files copied to remote clipboard");
      shellRef.current?.focus();
    } catch (err) {
      const message = err instanceof Error ? err.message : "Dropped files could not be copied";
      setStatus("file upload failed");
      onError(message);
    } finally {
      setDropBusy(false);
    }
  };

  const handleDragEnter = (event: DragEvent<HTMLDivElement>) => {
    if (!dropHasFiles(event.dataTransfer)) {
      return;
    }
    event.preventDefault();
    dragDepthRef.current += 1;
    setDragActive(true);
  };

  const handleDragLeave = (event: DragEvent<HTMLDivElement>) => {
    if (!dropHasFiles(event.dataTransfer)) {
      return;
    }
    event.preventDefault();
    dragDepthRef.current = Math.max(0, dragDepthRef.current - 1);
    if (dragDepthRef.current === 0) {
      setDragActive(false);
    }
  };

  const handleDragOver = (event: DragEvent<HTMLDivElement>) => {
    if (!dropHasFiles(event.dataTransfer)) {
      return;
    }
    event.preventDefault();
    event.dataTransfer.dropEffect = "copy";
  };

  return (
    <section className="flex h-full min-h-0 flex-col bg-neutral-950">
      <div className="flex h-11 shrink-0 items-center justify-between border-b border-neutral-800 bg-neutral-950 px-3">
        <div className="flex min-w-0 items-center gap-2">
          {connected ? (
            <Wifi className="h-4 w-4 shrink-0 text-emerald-400" aria-hidden="true" />
          ) : (
            <WifiOff className="h-4 w-4 shrink-0 text-amber-400" aria-hidden="true" />
          )}
          <span className="truncate text-sm font-medium text-neutral-100">
            {displaySessionName(session)}
          </span>
          <span className="hidden truncate text-xs text-neutral-500 sm:inline">{status}</span>
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
        ref={shellRef}
        tabIndex={0}
        className={[
          "relative min-h-0 flex-1 touch-none select-none overflow-hidden bg-black outline-none",
          focused ? "ring-2 ring-emerald-500" : "focus-visible:ring-2 focus-visible:ring-emerald-500",
        ].join(" ")}
        onFocus={() => setFocused(true)}
        onBlur={() => {
          setFocused(false);
          sendKeyReset();
        }}
        onPointerDown={(event) => {
          event.currentTarget.focus();
          event.currentTarget.setPointerCapture(event.pointerId);
          audioRef.current?.resume();
          resumeGUIAudio();
          sendPointer(event, true);
        }}
        onPointerMove={(event) => sendPointer(event)}
        onPointerUp={(event) => sendPointer(event, false)}
        onPointerCancel={(event) => sendPointer(event, false)}
        onContextMenu={(event) => event.preventDefault()}
        onWheel={sendWheel}
        onKeyDown={(event) => sendKey(event, true)}
        onKeyUp={(event) => sendKey(event, false)}
        onDragEnter={handleDragEnter}
        onDragLeave={handleDragLeave}
        onDragOver={handleDragOver}
        onDrop={(event) => {
          void uploadDroppedFiles(event);
        }}
      >
        <CanvasHost
          key={session.id}
          sessionId={session.id}
          onRenderer={setVideoRenderer}
          onStatus={setStatus}
        />
        {dragActive || dropBusy ? (
          <div className="pointer-events-none absolute inset-4 grid place-items-center border border-dashed border-emerald-400/70 bg-black/45 text-sm font-medium text-emerald-100">
            {dropBusy ? "Copying files to remote clipboard" : "Drop files to copy to remote clipboard"}
          </div>
        ) : null}
      </div>
    </section>
  );
}

type GUIVideoRenderer = {
  renderBinaryFrame(buffer: ArrayBuffer): void;
  renderLegacyFrame(frame: Extract<GUIServerMessage, { type: "frame" }>): void;
  resize(width: number, height: number): void;
  close(): void;
};

type CanvasHostProps = {
  sessionId: string;
  onRenderer: (renderer: GUIVideoRenderer | null) => void;
  onStatus: (status: string) => void;
};

function CanvasHost({ onRenderer, onStatus }: CanvasHostProps) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const rendererRef = useRef<GUIVideoRenderer | null>(null);
  const resizeObserverRef = useRef<ResizeObserver | null>(null);

  const cleanup = useCallback(() => {
    resizeObserverRef.current?.disconnect();
    resizeObserverRef.current = null;
    rendererRef.current?.close();
    rendererRef.current = null;
    canvasRef.current = null;
    onRenderer(null);
  }, [onRenderer]);

  const attachCanvas = useCallback(
    (canvas: HTMLCanvasElement | null) => {
      if (canvasRef.current === canvas) {
        return;
      }
      cleanup();
      if (!canvas) {
        return;
      }

      canvasRef.current = canvas;
      const renderer = createVideoRenderer(canvas, onStatus);
      rendererRef.current = renderer;
      onRenderer(renderer);

      const resize = () => resizeRendererToCanvas(canvas, renderer);
      resize();
      const resizeObserver = new ResizeObserver(resize);
      resizeObserver.observe(canvas);
      resizeObserverRef.current = resizeObserver;
    },
    [cleanup, onRenderer, onStatus],
  );

  useEffect(() => cleanup, [cleanup]);

  return (
    <canvas
      ref={attachCanvas}
      className="block h-full w-full"
      style={{
        cursor:
          "url('data:image/svg+xml,%3Csvg%20xmlns=%22http://www.w3.org/2000/svg%22%20width=%227%22%20height=%227%22%20viewBox=%220%200%207%207%22%3E%3Ccircle%20cx=%223.5%22%20cy=%223.5%22%20r=%223%22%20fill=%22%23000%22/%3E%3Ccircle%20cx=%223.5%22%20cy=%223.5%22%20r=%221.5%22%20fill=%22%23fff%22/%3E%3C/svg%3E') 3 3, crosshair",
      }}
    />
  );
}

function createVideoRenderer(canvas: HTMLCanvasElement, onStatus: (status: string) => void): GUIVideoRenderer {
  if (canTransferVisibleCanvas(canvas)) {
    try {
      const worker = new Worker(new URL("../workers/guiVideoWorker.ts", import.meta.url), {
        type: "module",
      });
      worker.onmessage = (event) => {
        const message = event.data;
        if (message.type === "error") {
          onStatus(message.message ?? "video worker unavailable");
        }
      };
      const offscreen = canvas.transferControlToOffscreen();
      worker.postMessage({ type: "init", mode: "canvas", canvas: offscreen }, [offscreen]);
      return workerVideoRenderer(worker);
    } catch (err) {
      onStatus(err instanceof Error ? err.message : "video worker unavailable");
    }
  }

  if (canUseBitmapVideoWorker()) {
    try {
      const context = canvas.getContext("2d", { alpha: false });
      if (context) {
        const worker = new Worker(new URL("../workers/guiVideoWorker.ts", import.meta.url), {
          type: "module",
        });
        worker.onmessage = (event) => {
          const message = event.data;
          if (message.type === "bitmap") {
            drawBitmapToCanvas(canvas, context, message.bitmap);
            return;
          }
          if (message.type === "error") {
            onStatus(message.message ?? "video worker unavailable");
          }
        };
        worker.postMessage({ type: "init", mode: "bitmap" });
        return workerVideoRenderer(worker);
      }
    } catch (err) {
      onStatus(err instanceof Error ? err.message : "video worker unavailable");
    }
  }

  return mainThreadVideoRenderer(canvas, onStatus);
}

function workerVideoRenderer(worker: Worker): GUIVideoRenderer {
  return {
    renderBinaryFrame(buffer) {
      worker.postMessage({ type: "frame", data: buffer }, [buffer]);
    },
    renderLegacyFrame() {
      // Legacy JSON frame payloads are only used by old servers. The current
      // backend sends binary JPEG frames, which keeps frame data off the main
      // thread and allows this worker-owned canvas path to stay hot.
    },
    resize(width, height) {
      worker.postMessage({ type: "resize", width, height });
    },
    close() {
      worker.terminate();
    },
  };
}

function mainThreadVideoRenderer(canvas: HTMLCanvasElement, onStatus: (status: string) => void): GUIVideoRenderer {
  const context = canvas.getContext("2d", { alpha: false });
  const framebuffer = document.createElement("canvas");
  const framebufferContext = framebuffer.getContext("2d", { alpha: false });
  let closed = false;
  let queue = Promise.resolve();

  if (!context || !framebufferContext) {
    onStatus("canvas unavailable");
    return {
      renderBinaryFrame() {},
      renderLegacyFrame() {},
      resize() {},
      close() {
        closed = true;
      },
    };
  }

  const ensureFramebuffer = (width: number, height: number) => {
    const nextWidth = Math.max(1, width);
    const nextHeight = Math.max(1, height);
    if (framebuffer.width !== nextWidth || framebuffer.height !== nextHeight) {
      framebuffer.width = nextWidth;
      framebuffer.height = nextHeight;
      framebufferContext.fillStyle = "#050505";
      framebufferContext.fillRect(0, 0, framebuffer.width, framebuffer.height);
    }
  };

  const renderFramebuffer = () => {
    if (closed || framebuffer.width <= 0 || framebuffer.height <= 0) {
      return;
    }
    context.imageSmoothingEnabled = false;
    context.fillStyle = "#050505";
    context.fillRect(0, 0, canvas.width, canvas.height);
    context.drawImage(framebuffer, 0, 0, canvas.width, canvas.height);
  };

  const drawFullFrame = (image: CanvasImageSource, width: number, height: number) => {
    ensureFramebuffer(width, height);
    framebufferContext.imageSmoothingEnabled = false;
    framebufferContext.drawImage(image, 0, 0, framebuffer.width, framebuffer.height);
    renderFramebuffer();
  };

  const drawRectFrame = (image: CanvasImageSource, frame: GUIBinaryFrame) => {
    ensureFramebuffer(frame.screenWidth, frame.screenHeight);
    framebufferContext.imageSmoothingEnabled = false;
    framebufferContext.drawImage(image, frame.x, frame.y, frame.width, frame.height);
    renderFramebuffer();
  };

  const drawBinaryFrame = async (buffer: ArrayBuffer) => {
    const frame = parseBinaryFrame(buffer);
    if (!frame || frame.kind === "audio") {
      return;
    }
    const blob = new Blob([frame.data], { type: "image/jpeg" });
    const image = await decodeJPEGFrame(blob);
    try {
      if (closed) {
        return;
      }
      if (frame.kind === "full") {
        drawFullFrame(image, frame.screenWidth, frame.screenHeight);
      } else {
        drawRectFrame(image, frame);
      }
    } finally {
      closeDecodedImage(image);
    }
  };

  return {
    renderBinaryFrame(buffer) {
      queue = queue.then(() => drawBinaryFrame(buffer)).catch(() => onStatus("frame decode failed"));
    },
    renderLegacyFrame(frame) {
      const image = new Image();
      image.onload = () => {
        if (!closed) {
          drawFullFrame(image, frame.width, frame.height);
        }
      };
      image.src = `data:image/${frame.encoding};base64,${frame.data}`;
    },
    resize(width, height) {
      if (canvas.width !== width || canvas.height !== height) {
        canvas.width = width;
        canvas.height = height;
      }
      renderFramebuffer();
    },
    close() {
      closed = true;
    },
  };
}

function resizeRendererToCanvas(canvas: HTMLCanvasElement, renderer: GUIVideoRenderer) {
  const rect = canvas.getBoundingClientRect();
  const dpr = window.devicePixelRatio || 1;
  renderer.resize(
    Math.max(1, Math.floor(rect.width * dpr)),
    Math.max(1, Math.floor(rect.height * dpr)),
  );
}

function drawBitmapToCanvas(canvas: HTMLCanvasElement, context: CanvasRenderingContext2D, bitmap: ImageBitmap) {
  try {
    context.imageSmoothingEnabled = false;
    context.fillStyle = "#050505";
    context.fillRect(0, 0, canvas.width, canvas.height);
    context.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
  } finally {
    bitmap.close();
  }
}

function parseServerMessage(data: string): GUIServerMessage | null {
  try {
    return JSON.parse(data) as GUIServerMessage;
  } catch {
    return null;
  }
}

function workerTransport(worker: Worker): GUITransport {
  return {
    readyState: WebSocket.CONNECTING,
    send(data: string) {
      worker.postMessage({ type: "send", data });
    },
    close() {
      this.readyState = WebSocket.CLOSING;
      worker.postMessage({ type: "close" });
    },
  };
}

function canTransferVisibleCanvas(canvas: HTMLCanvasElement) {
  if (typeof Worker === "undefined") {
    return false;
  }
  if (typeof OffscreenCanvas === "undefined") {
    return false;
  }
  return typeof canvas.transferControlToOffscreen === "function";
}

function canUseBitmapVideoWorker() {
  if (typeof Worker === "undefined") {
    return false;
  }
  if (typeof OffscreenCanvas === "undefined") {
    return false;
  }
  if (typeof createImageBitmap !== "function") {
    return false;
  }
  return true;
}

function binaryFrameDimensions(buffer: ArrayBuffer) {
  if (buffer.byteLength < 12) {
    return null;
  }
  const view = new DataView(buffer);
  const magic =
    String.fromCharCode(view.getUint8(0)) +
    String.fromCharCode(view.getUint8(1)) +
    String.fromCharCode(view.getUint8(2)) +
    String.fromCharCode(view.getUint8(3));
  if (magic !== "WSGF" && magic !== "WSGR") {
    return null;
  }
  return {
    width: Math.max(1, view.getUint32(4)),
    height: Math.max(1, view.getUint32(8)),
  };
}

function parseBinaryFrame(buffer: ArrayBuffer): GUIBinaryFrame | null {
  const header = parseBinaryFrameHeader(buffer);
  if (!header) {
    return null;
  }
  if (header.fullFrame) {
    return header.fullFrame;
  }
  const { magic, view } = header;
  if (magic === "WSAU") {
    const audioFrame = parseBinaryAudioFrame(buffer);
    return {
      kind: "audio",
      screenWidth: 0,
      screenHeight: 0,
      x: 0,
      y: 0,
      width: 0,
      height: 0,
      data: audioFrame?.data ?? buffer.slice(binaryAudioFrameHeaderLen),
    };
  }

  if (magic !== "WSGR" || buffer.byteLength < 28) {
    return null;
  }

  return {
    kind: "rect",
    screenWidth: view.getUint32(4),
    screenHeight: view.getUint32(8),
    x: view.getUint32(12),
    y: view.getUint32(16),
    width: view.getUint32(20),
    height: view.getUint32(24),
    data: buffer.slice(28),
  };
}

function parseBinaryFrameHeader(buffer: ArrayBuffer) {
  if (buffer.byteLength < 12) {
    return null;
  }
  const view = new DataView(buffer);
  const magic =
    String.fromCharCode(view.getUint8(0)) +
    String.fromCharCode(view.getUint8(1)) +
    String.fromCharCode(view.getUint8(2)) +
    String.fromCharCode(view.getUint8(3));
  if (magic === "WSGF") {
    const width = view.getUint32(4);
    const height = view.getUint32(8);
    return {
      magic,
      view,
      fullFrame: {
        kind: "full" as const,
        screenWidth: width,
        screenHeight: height,
        x: 0,
        y: 0,
        width,
        height,
        data: buffer.slice(12),
      },
    };
  }
  return { magic, view, fullFrame: null };
}

function parseBinaryAudioFrame(buffer: ArrayBuffer) {
  if (buffer.byteLength < binaryAudioFrameHeaderLen) {
    return null;
  }
  const view = new DataView(buffer);
  if (
    view.getUint8(0) !== 0x57 ||
    view.getUint8(1) !== 0x53 ||
    view.getUint8(2) !== 0x41 ||
    view.getUint8(3) !== 0x55
  ) {
    return null;
  }
  return {
    sampleRate: view.getUint32(4),
    channels: view.getUint16(8),
    data: buffer.slice(binaryAudioFrameHeaderLen),
  };
}

async function decodeJPEGFrame(blob: Blob): Promise<ImageBitmap | HTMLImageElement> {
  if (typeof createImageBitmap === "function") {
    return createImageBitmap(blob);
  }

  return new Promise((resolve, reject) => {
    const url = URL.createObjectURL(blob);
    const image = new Image();
    image.onload = () => {
      URL.revokeObjectURL(url);
      resolve(image);
    };
    image.onerror = () => {
      URL.revokeObjectURL(url);
      reject(new Error("Could not decode GUI frame"));
    };
    image.src = url;
  });
}

function closeDecodedImage(image: ImageBitmap | HTMLImageElement) {
  if ("close" in image) {
    image.close();
  }
}

function pointerToRemote(
  event: PointerEvent<HTMLDivElement> | WheelEvent<HTMLDivElement>,
  remoteSize: { width: number; height: number },
) {
  const rect = event.currentTarget.getBoundingClientRect();
  return {
    x: Math.max(0, Math.min(remoteSize.width, Math.floor(((event.clientX - rect.left) / rect.width) * remoteSize.width))),
    y: Math.max(0, Math.min(remoteSize.height, Math.floor(((event.clientY - rect.top) / rect.height) * remoteSize.height))),
  };
}

function pointerButton(button: number) {
  if (button === 1) {
    return 2;
  }
  if (button === 2) {
    return 3;
  }
  return 1;
}

function wheelDeltaFactor(deltaMode: number) {
  if (deltaMode === 1) {
    return wheelLinePixels;
  }
  if (deltaMode === 2) {
    return wheelPagePixels;
  }
  return 1;
}

type DroppedFile = {
  file: File;
  path: string;
};

type WebKitEntry = {
  isFile: boolean;
  isDirectory: boolean;
  name: string;
};

type WebKitFileEntry = WebKitEntry & {
  isFile: true;
  file: (success: (file: File) => void, failure?: (error: DOMException) => void) => void;
};

type WebKitDirectoryEntry = WebKitEntry & {
  isDirectory: true;
  createReader: () => {
    readEntries: (
      success: (entries: WebKitEntry[]) => void,
      failure?: (error: DOMException) => void,
    ) => void;
  };
};

type DataTransferItemWithEntry = DataTransferItem & {
  webkitGetAsEntry?: () => unknown;
};

function dropHasFiles(dataTransfer: DataTransfer) {
  return Array.from(dataTransfer.types).includes("Files");
}

async function filesFromDrop(dataTransfer: DataTransfer): Promise<DroppedFile[]> {
  const items = Array.from(dataTransfer.items ?? []) as DataTransferItemWithEntry[];
  const entries: WebKitEntry[] = [];
  for (const item of items) {
    const entry = item.webkitGetAsEntry?.();
    if (isWebKitEntry(entry)) {
      entries.push(entry);
    }
  }

  if (entries.length > 0) {
    const nested = await Promise.all(entries.map((entry) => filesFromEntry(entry, "")));
    return nested.flat();
  }

  return Array.from(dataTransfer.files ?? []).map((file) => ({
    file,
    path: file.webkitRelativePath || file.name,
  }));
}

function isWebKitEntry(value: unknown): value is WebKitEntry {
  if (!value || typeof value !== "object") {
    return false;
  }
  const entry = value as Partial<WebKitEntry>;
  return typeof entry.name === "string" && (entry.isFile === true || entry.isDirectory === true);
}

async function filesFromEntry(entry: WebKitEntry, parentPath: string): Promise<DroppedFile[]> {
  const path = parentPath ? `${parentPath}/${entry.name}` : entry.name;
  if (entry.isFile) {
    const file = await fileFromEntry(entry as WebKitFileEntry);
    return [{ file, path }];
  }
  if (!entry.isDirectory) {
    return [];
  }

  const children = await readDirectoryEntries(entry as WebKitDirectoryEntry);
  const nested = await Promise.all(children.map((child) => filesFromEntry(child, path)));
  return nested.flat();
}

function fileFromEntry(entry: WebKitFileEntry) {
  return new Promise<File>((resolve, reject) => {
    entry.file(resolve, reject);
  });
}

async function readDirectoryEntries(entry: WebKitDirectoryEntry) {
  const reader = entry.createReader();
  const entries: WebKitEntry[] = [];
  for (;;) {
    const batch = await new Promise<WebKitEntry[]>((resolve, reject) => {
      reader.readEntries(resolve, reject);
    });
    if (batch.length === 0) {
      return entries;
    }
    entries.push(...batch);
  }
}

function keyStateID(event: KeyboardEvent<HTMLDivElement>) {
  return event.code || `key:${event.key}:${event.location}`;
}

function shouldForwardKeyEvent(
  event: KeyboardEvent<HTMLDivElement>,
  down: boolean,
  alreadyForwarded: boolean,
) {
  if (event.nativeEvent.isComposing || ignoredBrowserKeys.has(event.key)) {
    return false;
  }

  if (!down) {
    // Browsers can emit modifier keyup events after focus changes even when
    // this viewer never forwarded the matching keydown. Do not send those
    // orphan releases to the backend; `keyReset` handles focus loss.
    return alreadyForwarded;
  }

  if (event.repeat) {
    return !modifierCodes.has(event.code);
  }

  return !alreadyForwarded;
}

const ignoredBrowserKeys = new Set(["Dead", "Fn", "FnLock", "Hyper", "Process", "Symbol", "SymbolLock", "Unidentified"]);

const modifierCodes = new Set([
  "AltLeft",
  "AltRight",
  "ControlLeft",
  "ControlRight",
  "MetaLeft",
  "MetaRight",
  "ShiftLeft",
  "ShiftRight",
]);

function displaySessionName(session: SessionInfo) {
  return session.name.replace(/^GUI(\s+\d+)$/i, "Desktop$1");
}
