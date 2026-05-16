import { WebShellSocket } from "../lib/secureWebSocket";

type WorkerCommand =
  | { type: "connect"; url: string }
  | { type: "send"; data: string | ArrayBuffer }
  | { type: "close" };

let socket: WebShellSocket | null = null;
let connectionID = 0;
const post = self.postMessage.bind(self) as (message: unknown, transfer?: Transferable[]) => void;

self.onmessage = (event: MessageEvent<WorkerCommand>) => {
  const message = event.data;
  if (message.type === "connect") {
    void connect(message.url);
    return;
  }
  if (message.type === "send") {
    socket?.send(message.data);
    return;
  }
  if (message.type === "close") {
    const current = socket;
    socket = null;
    current?.close();
  }
};

async function connect(url: string) {
  socket?.close();
  const id = ++connectionID;
  const next = new WebShellSocket(url);
  socket = next;
  next.binaryType = "arraybuffer";

  next.addEventListener("open", () => {
    if (socket !== next || id !== connectionID) {
      next.close();
      return;
    }
    post({ type: "open" });
  });
  next.addEventListener("close", (event) => {
    if (socket !== next || id !== connectionID) {
      return;
    }
    post({
      type: "close",
      code: event.code,
      reason: event.reason,
      wasClean: event.wasClean,
    });
  });
  next.addEventListener("error", () => {
    if (socket !== next || id !== connectionID || next.clientClosed) {
      return;
    }
    post({ type: "error" });
  });
  next.addEventListener("message", (event) => {
    if (socket !== next || id !== connectionID) {
      return;
    }
    void handleSocketMessage(event.data);
  });

  try {
    await next.connect();
  } catch (err) {
    if (socket !== next || id !== connectionID || next.clientClosed) {
      return;
    }
    post({ type: "error", message: err instanceof Error ? err.message : "websocket connection failed" });
    next.close();
  }
}

async function handleSocketMessage(data: string | ArrayBuffer | Blob) {
  if (typeof data === "string") {
    post({ type: "message", data });
    return;
  }

  const buffer = data instanceof Blob ? await data.arrayBuffer() : data;
  const kind = binaryKind(buffer);
  if (kind === "audio") {
    post({ type: "audio", data: buffer }, [buffer]);
    return;
  }
  if (kind === "video") {
    post({ type: "video", data: buffer }, [buffer]);
    return;
  }
  post({ type: "binary", data: buffer }, [buffer]);
}

function binaryKind(buffer: ArrayBuffer) {
  if (buffer.byteLength < 4) {
    return "unknown";
  }
  const view = new DataView(buffer);
  const magic =
    String.fromCharCode(view.getUint8(0)) +
    String.fromCharCode(view.getUint8(1)) +
    String.fromCharCode(view.getUint8(2)) +
    String.fromCharCode(view.getUint8(3));
  if (magic === "WSAU") {
    return "audio";
  }
  if (magic === "WSGF" || magic === "WSGR") {
    return "video";
  }
  return "unknown";
}
