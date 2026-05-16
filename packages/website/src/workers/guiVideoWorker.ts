let output: OffscreenCanvas | null = null;
let outputContext: OffscreenCanvasRenderingContext2D | null = null;
let framebuffer: OffscreenCanvas | null = null;
let framebufferContext: OffscreenCanvasRenderingContext2D | null = null;
let outputWidth = 1;
let outputHeight = 1;
let outputMode: "bitmap" | "canvas" = "bitmap";

const post = self.postMessage.bind(self) as (message: unknown, transfer?: Transferable[]) => void;

self.onmessage = (event: MessageEvent) => {
  const message = event.data;
  if (message.type === "init") {
    initialize(message);
    return;
  }
  if (message.type === "resize") {
    outputWidth = Math.max(1, message.width);
    outputHeight = Math.max(1, message.height);
    ensureOutput();
    renderOutput();
    return;
  }
  if (message.type === "frame") {
    void drawFrame(message.data).catch((err) => {
      post({ type: "error", message: err instanceof Error ? err.message : "video worker failed" });
    });
  }
};

function initialize(message: { mode?: "bitmap" | "canvas"; canvas?: OffscreenCanvas }) {
  if (typeof OffscreenCanvas === "undefined" || typeof createImageBitmap !== "function") {
    post({ type: "error", message: "video worker unavailable" });
    return;
  }
  outputMode = message.mode === "canvas" ? "canvas" : "bitmap";
  output = outputMode === "canvas" && message.canvas ? message.canvas : new OffscreenCanvas(outputWidth, outputHeight);
  outputContext = output.getContext("2d", { alpha: false });
  framebuffer = new OffscreenCanvas(1, 1);
  framebufferContext = framebuffer.getContext("2d", { alpha: false });
  if (!outputContext || !framebufferContext) {
    post({ type: "error", message: "video worker canvas unavailable" });
    return;
  }
  if (outputMode === "bitmap" && typeof output.transferToImageBitmap !== "function") {
    post({ type: "error", message: "video worker canvas unavailable" });
    return;
  }
  ensureOutput();
}

async function drawFrame(buffer: ArrayBuffer) {
  if (!output || !outputContext || !framebuffer || !framebufferContext) {
    return;
  }

  const frame = parseFrame(buffer);
  if (!frame) {
    return;
  }

  const blob = new Blob([frame.data], { type: "image/jpeg" });
  const image = await createImageBitmap(blob);
  try {
    ensureFramebuffer(frame.screenWidth, frame.screenHeight);
    framebufferContext.imageSmoothingEnabled = false;
    if (frame.kind === "full") {
      framebufferContext.drawImage(image, 0, 0, framebuffer.width, framebuffer.height);
    } else {
      framebufferContext.drawImage(image, frame.x, frame.y, frame.width, frame.height);
    }
    renderOutput();
  } finally {
    image.close();
  }
}

function ensureOutput() {
  if (!output || !outputContext) {
    return;
  }
  if (output.width === outputWidth && output.height === outputHeight) {
    return;
  }
  output.width = outputWidth;
  output.height = outputHeight;
  outputContext.fillStyle = "#050505";
  outputContext.fillRect(0, 0, output.width, output.height);
}

function ensureFramebuffer(width: number, height: number) {
  if (!framebuffer || !framebufferContext) {
    return;
  }
  const nextWidth = Math.max(1, width);
  const nextHeight = Math.max(1, height);
  if (framebuffer.width === nextWidth && framebuffer.height === nextHeight) {
    return;
  }
  framebuffer.width = nextWidth;
  framebuffer.height = nextHeight;
  framebufferContext.fillStyle = "#050505";
  framebufferContext.fillRect(0, 0, framebuffer.width, framebuffer.height);
}

function renderOutput() {
  if (!output || !outputContext || !framebuffer) {
    return;
  }
  ensureOutput();
  outputContext.imageSmoothingEnabled = false;
  outputContext.fillStyle = "#050505";
  outputContext.fillRect(0, 0, output.width, output.height);
  outputContext.drawImage(framebuffer, 0, 0, output.width, output.height);
  if (outputMode === "bitmap") {
    const bitmap = output.transferToImageBitmap();
    post({ type: "bitmap", bitmap }, [bitmap]);
  }
}

function parseFrame(buffer: ArrayBuffer) {
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
      kind: "full",
      screenWidth: width,
      screenHeight: height,
      x: 0,
      y: 0,
      width,
      height,
      data: buffer.slice(12),
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
