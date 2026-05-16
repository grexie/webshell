import { x25519 } from "@noble/curves/ed25519";
import { base64ToBytes, bytesToBase64 } from "@/lib/sshKey";

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();
const envelopeMagic = new Uint8Array([0x57, 0x53, 0x45, 0x31]); // WSE1
const envelopeHeaderLength = 28;
const kindText = 1;
const kindBinary = 2;
const flagCompressed = 1;

type ServerHello = {
  type: "e2ee.serverHello";
  version: number;
  publicKey: string;
  cipher: string;
  keyExchange: string;
  compression: string;
};

type ClientHello = {
  type: "e2ee.clientHello";
  version: number;
  publicKey: string;
};

type SecureState = {
  sendKey: CryptoKey;
  recvKey: CryptoKey;
  sendSeq: bigint;
  recvSeq: bigint;
  recvReady: boolean;
};

export class WebShellSocket extends EventTarget {
  readonly raw: WebSocket;
  readyState: number = WebSocket.CONNECTING;
  binaryType: BinaryType = "arraybuffer";
  private closeRequested = false;
  private secure: SecureState | null = null;
  private e2eeEnabled = false;
  private sendQueue = Promise.resolve();
  private receiveQueue = Promise.resolve();

  constructor(url: string) {
    super();
    this.raw = new WebSocket(url);
    this.raw.binaryType = "arraybuffer";
    this.raw.addEventListener("close", (event) => {
      this.readyState = this.raw.readyState;
      this.dispatchEvent(
        new CloseEvent("close", {
          code: event.code,
          reason: event.reason,
          wasClean: event.wasClean,
        }),
      );
    });
    this.raw.addEventListener("error", () => this.dispatchEvent(new Event("error")));
  }

  get clientClosed() {
    return this.closeRequested;
  }

  override addEventListener(
    type: "message",
    callback: (this: WebShellSocket, event: MessageEvent) => void,
    options?: AddEventListenerOptions | boolean,
  ): void;
  override addEventListener(
    type: "close",
    callback: (this: WebShellSocket, event: CloseEvent) => void,
    options?: AddEventListenerOptions | boolean,
  ): void;
  override addEventListener(
    type: "open" | "error",
    callback: (this: WebShellSocket, event: Event) => void,
    options?: AddEventListenerOptions | boolean,
  ): void;
  override addEventListener(
    type: string,
    callback: any,
    options?: AddEventListenerOptions | boolean,
  ) {
    super.addEventListener(type, callback, options);
  }

  async connect() {
    const bufferedRawMessages: MessageEvent[] = [];
    let bufferedMessageWaiter: ((event: MessageEvent) => void) | null = null;
    let bufferedMessageRejecter: ((err: Error) => void) | null = null;
    const captureRawMessage = (event: MessageEvent) => {
      if (bufferedMessageWaiter) {
        const resolve = bufferedMessageWaiter;
        bufferedMessageWaiter = null;
        bufferedMessageRejecter = null;
        resolve(event);
        return;
      }
      bufferedRawMessages.push(event);
    };
    const readBufferedRawMessage = () => {
      if (bufferedRawMessages.length > 0) {
        return Promise.resolve(bufferedRawMessages.shift()!);
      }
      return new Promise<MessageEvent>((resolve, reject) => {
        bufferedMessageWaiter = resolve;
        bufferedMessageRejecter = reject;
      });
    };
    const unreadBufferedRawMessage = (event: MessageEvent) => {
      bufferedRawMessages.unshift(event);
    };
    const readBufferedRawMessageBefore = (timeoutMs: number) =>
      new Promise<MessageEvent | null>((resolve, reject) => {
        if (bufferedRawMessages.length > 0) {
          resolve(bufferedRawMessages.shift()!);
          return;
        }
        const onMessage = (event: MessageEvent) => {
          globalThis.clearTimeout(timeout);
          resolve(event);
        };
        const timeout = globalThis.setTimeout(() => {
          if (bufferedMessageWaiter === onMessage) {
            bufferedMessageWaiter = null;
            bufferedMessageRejecter = null;
          }
          resolve(null);
        }, timeoutMs);
        bufferedMessageWaiter = onMessage;
        bufferedMessageRejecter = (err) => {
          globalThis.clearTimeout(timeout);
          reject(err);
        };
      });

    // The server can send e2ee.serverHello immediately after the websocket
    // upgrade. Capture raw messages from the start so a fast server hello cannot
    // race ahead of the raw open handler.
    this.raw.addEventListener("message", captureRawMessage);

    const openPromise = new Promise<void>((resolve, reject) => {
      if (this.raw.readyState === WebSocket.OPEN) {
        resolve();
        return;
      }
      this.raw.addEventListener("open", () => resolve(), { once: true });
      this.raw.addEventListener("error", () => reject(new Error("websocket connection failed")), { once: true });
      this.raw.addEventListener(
        "close",
        () => {
          bufferedMessageRejecter?.(new Error("websocket closed before handshake"));
          reject(new Error("websocket closed before open"));
        },
        { once: true },
      );
    });

    try {
      await openPromise;
      if (this.closeRequested) {
        this.raw.close();
        throw new Error("websocket closed");
      }

      // Do not block clientHello on an HTTP feature request. WebSockets count
      // toward browser per-origin connection pressure, and a feature fetch can
      // be delayed behind sockets that are themselves waiting for clientHello.
      const firstMessage = await readBufferedRawMessageBefore(1500);
      if (firstMessage && isServerHello(firstMessage)) {
        this.e2eeEnabled = true;
        await this.performHandshake(firstMessage);
      } else {
        if (firstMessage) {
          unreadBufferedRawMessage(firstMessage);
        }
        this.e2eeEnabled = false;
      }

      if (this.closeRequested) {
        this.raw.close();
        throw new Error("websocket closed");
      }
    } finally {
      this.raw.removeEventListener("message", captureRawMessage);
    }

    this.readyState = WebSocket.OPEN;
    this.raw.addEventListener("message", (event) => {
      // AEAD decrypt is async and GUI/htop can deliver several websocket frames
      // at once. Preserve arrival order so the monotonic sequence check remains
      // meaningful instead of racing decrypt completions against each other.
      this.receiveQueue = this.receiveQueue
        .then(() => this.handleMessage(event.data))
        .catch(() => {
          this.close();
        });
    });
    for (const event of bufferedRawMessages) {
      this.receiveQueue = this.receiveQueue
        .then(() => this.handleMessage(event.data))
        .catch(() => {
          this.close();
        });
    }
    this.dispatchEvent(new Event("open"));
  }

  send(data: string | ArrayBuffer | Blob | ArrayBufferView) {
    if (this.closeRequested) {
      return;
    }
    if (!this.secure) {
      this.raw.send(data);
      return;
    }

    this.sendQueue = this.sendQueue
      .then(async () => {
        const encrypted = await this.encrypt(data);
        this.raw.send(encrypted);
      })
      .catch(() => {
        this.close();
      });
  }

  close() {
    this.closeRequested = true;
    this.readyState = WebSocket.CLOSING;
    if (this.raw.readyState === WebSocket.CONNECTING) {
      // Closing a connecting WebSocket is correct during React cleanup, but
      // browsers report it as "closed before the connection is established".
      // Let it open, then close immediately, so intentional cleanup does not
      // appear as a GUI connection failure in the console or worker.
      this.raw.addEventListener("open", () => this.raw.close(), { once: true });
      return;
    }
    this.raw.close();
  }

  private async performHandshake(event: MessageEvent) {
    if (typeof event.data !== "string") {
      throw new Error("expected e2ee server hello");
    }
    const hello = JSON.parse(event.data) as ServerHello;
    if (hello.type !== "e2ee.serverHello" || hello.version !== 1) {
      throw new Error("invalid e2ee server hello");
    }

    if (hello.cipher !== "aes-256-gcm" || hello.keyExchange !== "x25519") {
      throw new Error("unsupported e2ee parameters");
    }

    const privateKey = x25519.utils.randomPrivateKey();
    const publicKey = x25519.getPublicKey(privateKey);
    const serverPublicKey = base64ToBytes(hello.publicKey);
    const sharedSecret = x25519.getSharedSecret(privateKey, serverPublicKey);
    const [sendKey, recvKey] = await deriveKeys(sharedSecret, publicKey, serverPublicKey);

    const clientHello: ClientHello = {
      type: "e2ee.clientHello",
      version: 1,
      publicKey: bytesToBase64(publicKey),
    };
    this.raw.send(JSON.stringify(clientHello));
    this.secure = {
      sendKey,
      recvKey,
      sendSeq: 0n,
      recvSeq: 0n,
      recvReady: false,
    };
  }

  private async handleMessage(data: string | ArrayBuffer | Blob) {
    if (!this.secure) {
      this.dispatchEvent(new MessageEvent("message", { data }));
      return;
    }

    const envelope = data instanceof Blob ? new Uint8Array(await data.arrayBuffer()) : new Uint8Array(data as ArrayBuffer);
    const decrypted = await this.decrypt(envelope);
    this.dispatchEvent(new MessageEvent("message", { data: decrypted }));
  }

  private async encrypt(data: string | ArrayBuffer | Blob | ArrayBufferView) {
    if (!this.secure) {
      throw new Error("secure websocket is not ready");
    }

    const text = typeof data === "string";
    const payload = text ? textEncoder.encode(data) : new Uint8Array(await normalizeBinary(data));
    const plaintext = new Uint8Array(1 + payload.length);
    plaintext[0] = text ? kindText : kindBinary;
    plaintext.set(payload, 1);

    const header = new Uint8Array(envelopeHeaderLength);
    header.set(envelopeMagic, 0);
    const view = new DataView(header.buffer);
    view.setBigUint64(8, this.secure.sendSeq);
    this.secure.sendSeq += 1n;
    crypto.getRandomValues(header.subarray(16, 28));

    const ciphertext = new Uint8Array(
      await crypto.subtle.encrypt(
        {
          name: "AES-GCM",
          iv: header.slice(16, 28),
          additionalData: header,
        },
        this.secure.sendKey,
        plaintext,
      ),
    );
    const envelope = new Uint8Array(header.length + ciphertext.length);
    envelope.set(header, 0);
    envelope.set(ciphertext, header.length);
    return envelope;
  }

  private async decrypt(envelope: Uint8Array) {
    if (!this.secure) {
      throw new Error("secure websocket is not ready");
    }
    if (envelope.byteLength < envelopeHeaderLength || !matchesMagic(envelope)) {
      throw new Error("invalid encrypted websocket envelope");
    }

    const flags = envelope[4];
    const header = envelope.slice(0, envelopeHeaderLength);
    const view = new DataView(header.buffer);
    const seq = view.getBigUint64(8);
    const expected = this.secure.recvReady ? this.secure.recvSeq + 1n : 0n;
    if (seq !== expected) {
      throw new Error("encrypted websocket sequence out of order");
    }

    const plaintext = new Uint8Array(
      await crypto.subtle.decrypt(
        {
          name: "AES-GCM",
          iv: header.slice(16, 28),
          additionalData: header,
        },
        this.secure.recvKey,
        envelope.slice(envelopeHeaderLength),
      ),
    );
    if (plaintext.length < 1) {
      throw new Error("empty encrypted websocket payload");
    }
    this.secure.recvSeq = seq;
    this.secure.recvReady = true;

    let payload = plaintext.slice(1);
    if ((flags & flagCompressed) !== 0) {
      payload = await decompressGzip(payload);
    }
    if (plaintext[0] === kindText) {
      return textDecoder.decode(payload);
    }
    if (plaintext[0] === kindBinary) {
      return payload.buffer.slice(payload.byteOffset, payload.byteOffset + payload.byteLength);
    }
    throw new Error("unknown encrypted websocket payload kind");
  }
}

export async function openWebShellSocket(url: string) {
  const socket = new WebShellSocket(url);
  void socket.connect().catch(() => {
    socket.close();
  });
  return socket;
}

async function deriveKeys(sharedSecret: Uint8Array, clientPublicKey: Uint8Array, serverPublicKey: Uint8Array) {
  const info = new Uint8Array("webshell e2ee v1".length + clientPublicKey.length + serverPublicKey.length);
  info.set(textEncoder.encode("webshell e2ee v1"), 0);
  info.set(clientPublicKey, "webshell e2ee v1".length);
  info.set(serverPublicKey, "webshell e2ee v1".length + clientPublicKey.length);

  const baseKey = await crypto.subtle.importKey("raw", arrayBufferFor(sharedSecret), "HKDF", false, ["deriveBits"]);
  const bits = await crypto.subtle.deriveBits(
    {
      name: "HKDF",
      hash: "SHA-256",
      salt: new Uint8Array(0),
      info,
    },
    baseKey,
    512,
  );
  const material = new Uint8Array(bits);
  const clientToServer = material.slice(0, 32);
  const serverToClient = material.slice(32, 64);
  return Promise.all([
    crypto.subtle.importKey("raw", arrayBufferFor(clientToServer), "AES-GCM", false, ["encrypt"]),
    crypto.subtle.importKey("raw", arrayBufferFor(serverToClient), "AES-GCM", false, ["decrypt"]),
  ]);
}

async function normalizeBinary(data: ArrayBuffer | Blob | ArrayBufferView) {
  if (data instanceof Blob) {
    return data.arrayBuffer();
  }
  if (data instanceof ArrayBuffer) {
    return data;
  }
  return data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength);
}

function matchesMagic(envelope: Uint8Array) {
  for (let i = 0; i < envelopeMagic.length; i++) {
    if (envelope[i] !== envelopeMagic[i]) {
      return false;
    }
  }
  return true;
}

function isServerHello(event: MessageEvent) {
  if (typeof event.data !== "string") {
    return false;
  }
  try {
    const parsed = JSON.parse(event.data) as Partial<ServerHello>;
    return parsed.type === "e2ee.serverHello" && parsed.version === 1;
  } catch {
    return false;
  }
}

function arrayBufferFor(bytes: Uint8Array): ArrayBuffer {
  return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer;
}

async function decompressGzip(bytes: Uint8Array) {
  if (typeof DecompressionStream === "undefined") {
    throw new Error("gzip decompression is not supported by this browser");
  }
  const stream = new Blob([arrayBufferFor(bytes)]).stream().pipeThrough(new DecompressionStream("gzip"));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}
