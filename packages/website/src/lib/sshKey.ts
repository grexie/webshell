import { ed25519 } from "@noble/curves/ed25519";

const storageKey = "webshell.encryptedSSHKey";
const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

type Ed25519SSHKey = {
  type: "ssh-ed25519";
  publicKey: string;
  comment: string;
  seed: Uint8Array;
};

type RSASSHKey = {
  type: "ssh-rsa";
  publicKey: string;
  comment: string;
  jwk: JsonWebKey;
};

export type UnlockedSSHKey = Ed25519SSHKey | RSASSHKey;

type PlainStoredKey = {
  type: UnlockedSSHKey["type"];
  publicKey: string;
  comment: string;
  seed?: string;
  jwk?: JsonWebKey;
};

export type EncryptedSSHKey = {
  version: 1;
  cipher: "AES-GCM";
  kdf: "PBKDF2-SHA256";
  iterations: number;
  salt: string;
  iv: string;
  ciphertext: string;
  publicKey: string;
  comment: string;
};

export function hasStoredKey() {
  return typeof window !== "undefined" && Boolean(window.localStorage.getItem(storageKey));
}

export function loadEncryptedKey(): EncryptedSSHKey | null {
  if (typeof window === "undefined") {
    return null;
  }

  const raw = window.localStorage.getItem(storageKey);
  if (!raw) {
    return null;
  }

  return JSON.parse(raw) as EncryptedSSHKey;
}

export function saveEncryptedKey(key: EncryptedSSHKey) {
  window.localStorage.setItem(storageKey, JSON.stringify(key));
}

export function clearEncryptedKey() {
  window.localStorage.removeItem(storageKey);
}

export async function encryptForStorage(key: UnlockedSSHKey, passphrase: string) {
  const iterations = 250_000;
  const salt = crypto.getRandomValues(new Uint8Array(16));
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const cryptoKey = await deriveEncryptionKey(passphrase, salt, iterations);
  const plaintext: PlainStoredKey = {
    type: key.type,
    publicKey: key.publicKey,
    comment: key.comment,
  };

  if (key.type === "ssh-ed25519") {
    plaintext.seed = bytesToBase64(key.seed);
  } else {
    plaintext.jwk = key.jwk;
  }

  const ciphertext = await crypto.subtle.encrypt(
    { name: "AES-GCM", iv: asBufferSource(iv) },
    cryptoKey,
    textEncoder.encode(JSON.stringify(plaintext)),
  );

  return {
    version: 1,
    cipher: "AES-GCM",
    kdf: "PBKDF2-SHA256",
    iterations,
    salt: bytesToBase64(salt),
    iv: bytesToBase64(iv),
    ciphertext: bytesToBase64(new Uint8Array(ciphertext)),
    publicKey: key.publicKey,
    comment: key.comment,
  } satisfies EncryptedSSHKey;
}

export async function decryptStoredKey(stored: EncryptedSSHKey, passphrase: string) {
  const salt = base64ToBytes(stored.salt);
  const iv = base64ToBytes(stored.iv);
  const cryptoKey = await deriveEncryptionKey(passphrase, salt, stored.iterations);
  const plaintext = await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: asBufferSource(iv) },
    cryptoKey,
    asBufferSource(base64ToBytes(stored.ciphertext)),
  );
  const decoded = JSON.parse(textDecoder.decode(plaintext)) as PlainStoredKey;

  if (decoded.type === "ssh-ed25519") {
    if (!decoded.seed) {
      throw new Error("Stored Ed25519 key is missing private key data");
    }
    return {
      type: decoded.type,
      publicKey: decoded.publicKey,
      comment: decoded.comment,
      seed: base64ToBytes(decoded.seed),
    } satisfies UnlockedSSHKey;
  }

  if (decoded.type === "ssh-rsa") {
    if (!decoded.jwk) {
      throw new Error("Stored RSA key is missing private key data");
    }
    return {
      type: decoded.type,
      publicKey: decoded.publicKey,
      comment: decoded.comment,
      jwk: decoded.jwk,
    } satisfies UnlockedSSHKey;
  }

  throw new Error(`Unsupported stored key type: ${decoded.type}`);
}

export function parseSSHPrivateKey(pem: string): UnlockedSSHKey {
  if (/-----BEGIN RSA PRIVATE KEY-----/.test(pem)) {
    return parsePKCS1RSAPrivateKey(pem);
  }
  if (/-----BEGIN OPENSSH PRIVATE KEY-----/.test(pem)) {
    return parseOpenSSHPrivateKey(pem);
  }
  if (/-----BEGIN .*PRIVATE KEY-----/.test(pem)) {
    throw new Error("This browser build supports OpenSSH and PKCS#1 RSA private keys");
  }
  throw new Error("Paste an SSH private key");
}

export function parseOpenSSHPrivateKey(pem: string): UnlockedSSHKey {
  const base64 = pemBlockBase64(pem, "OPENSSH PRIVATE KEY");
  const reader = new SSHReader(base64ToBytes(base64));
  const magic = reader.readBytes("openssh-key-v1\0".length);
  if (textDecoder.decode(magic) !== "openssh-key-v1\0") {
    throw new Error("Invalid OpenSSH private key");
  }

  const cipherName = reader.readStringText();
  const kdfName = reader.readStringText();
  reader.readString();
  const keyCount = reader.readUint32();
  if (cipherName !== "none" || kdfName !== "none") {
    throw new Error("Import an unencrypted OpenSSH key; WebShell will encrypt it for storage");
  }
  if (keyCount !== 1) {
    throw new Error("OpenSSH key files with multiple keys are not supported");
  }

  const publicBlob = reader.readString();
  const privateBlock = new SSHReader(reader.readString());
  const check1 = privateBlock.readUint32();
  const check2 = privateBlock.readUint32();
  if (check1 !== check2) {
    throw new Error("Invalid OpenSSH private key");
  }

  const keyType = privateBlock.readStringText();
  switch (keyType) {
    case "ssh-ed25519":
      return parseOpenSSHEd25519PrivateBlock(publicBlob, privateBlock);
    case "ssh-rsa":
      return parseOpenSSHRSAPrivateBlock(publicBlob, privateBlock);
    default:
      throw new Error(`Unsupported OpenSSH key type: ${keyType}`);
  }
}

export function parseOpenSSHEd25519PrivateKey(pem: string): UnlockedSSHKey {
  const key = parseOpenSSHPrivateKey(pem);
  if (key.type !== "ssh-ed25519") {
    throw new Error(`Expected an Ed25519 key, got ${key.type}`);
  }
  return key;
}

function parseOpenSSHEd25519PrivateBlock(
  publicBlob: Uint8Array,
  privateBlock: SSHReader,
): Ed25519SSHKey {
  const publicBytes = privateBlock.readString();
  const privateBytes = privateBlock.readString();
  const comment = privateBlock.readStringText();
  if (privateBytes.length < 32) {
    throw new Error("Invalid Ed25519 private key");
  }

  const seed = privateBytes.slice(0, 32);
  const publicKey = `ssh-ed25519 ${bytesToBase64(publicBlob)}${comment ? ` ${comment}` : ""}`;
  const expectedBlob = sshString("ssh-ed25519", publicBytes);
  if (!bytesEqual(publicBlob, expectedBlob)) {
    throw new Error("OpenSSH key public half is inconsistent");
  }

  return {
    type: "ssh-ed25519",
    publicKey,
    comment,
    seed,
  };
}

function parseOpenSSHRSAPrivateBlock(publicBlob: Uint8Array, privateBlock: SSHReader): RSASSHKey {
  const n = privateBlock.readString();
  const e = privateBlock.readString();
  const d = privateBlock.readString();
  const qi = privateBlock.readString();
  const p = privateBlock.readString();
  const q = privateBlock.readString();
  const comment = privateBlock.readStringText();
  const dp = bigintToUnsignedBytes(bytesToBigint(d) % (bytesToBigint(p) - 1n));
  const dq = bigintToUnsignedBytes(bytesToBigint(d) % (bytesToBigint(q) - 1n));
  const expectedBlob = sshString("ssh-rsa", toMPInt(e), toMPInt(n));

  if (!bytesEqual(publicBlob, expectedBlob)) {
    throw new Error("OpenSSH RSA key public half is inconsistent");
  }

  return rsaKeyFromComponents({
    n,
    e,
    d,
    p,
    q,
    dp,
    dq,
    qi,
    publicBlob,
    comment,
  });
}

function parsePKCS1RSAPrivateKey(pem: string): RSASSHKey {
  const der = base64ToBytes(pemBlockBase64(pem, "RSA PRIVATE KEY"));
  const reader = new DERReader(der);
  const sequence = new DERReader(reader.readConstructed(0x30));
  sequence.readInteger();
  const n = sequence.readInteger();
  const e = sequence.readInteger();
  const d = sequence.readInteger();
  const p = sequence.readInteger();
  const q = sequence.readInteger();
  const dp = sequence.readInteger();
  const dq = sequence.readInteger();
  const qi = sequence.readInteger();
  const publicBlob = sshString("ssh-rsa", toMPInt(e), toMPInt(n));

  return rsaKeyFromComponents({
    n,
    e,
    d,
    p,
    q,
    dp,
    dq,
    qi,
    publicBlob,
    comment: "",
  });
}

function rsaKeyFromComponents({
  n,
  e,
  d,
  p,
  q,
  dp,
  dq,
  qi,
  publicBlob,
  comment,
}: {
  n: Uint8Array;
  e: Uint8Array;
  d: Uint8Array;
  p: Uint8Array;
  q: Uint8Array;
  dp: Uint8Array;
  dq: Uint8Array;
  qi: Uint8Array;
  publicBlob: Uint8Array;
  comment: string;
}): RSASSHKey {
  return {
    type: "ssh-rsa",
    publicKey: `ssh-rsa ${bytesToBase64(publicBlob)}${comment ? ` ${comment}` : ""}`,
    comment,
    jwk: {
      kty: "RSA",
      n: bytesToBase64URL(unsignedBytes(n)),
      e: bytesToBase64URL(unsignedBytes(e)),
      d: bytesToBase64URL(unsignedBytes(d)),
      p: bytesToBase64URL(unsignedBytes(p)),
      q: bytesToBase64URL(unsignedBytes(q)),
      dp: bytesToBase64URL(unsignedBytes(dp)),
      dq: bytesToBase64URL(unsignedBytes(dq)),
      qi: bytesToBase64URL(unsignedBytes(qi)),
      ext: true,
      key_ops: ["sign"],
    },
  };
}

export async function signAuthChallenge(key: UnlockedSSHKey, nonce: Uint8Array) {
  const signature = await signSSHData(key, nonce, 0);
  return bytesToBase64(sshString(signature.format, base64ToBytes(signature.signature)));
}

export async function signAgentRequest(key: UnlockedSSHKey, data: Uint8Array, flags = 0) {
  return signSSHData(key, data, flags);
}

export function publicKeyBlobBase64(publicKey: string) {
  const parts = publicKey.trim().split(/\s+/);
  if (parts.length < 2) {
    throw new Error("Invalid SSH public key");
  }
  return parts[1];
}

export function base64ToBytes(value: string) {
  const binary = atob(value);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    out[i] = binary.charCodeAt(i);
  }
  return out;
}

export function bytesToBase64(bytes: Uint8Array) {
  let binary = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.slice(i, i + chunk));
  }
  return btoa(binary);
}

function sshString(...values: Array<string | Uint8Array>) {
  const encoded = values.map((value) =>
    typeof value === "string" ? textEncoder.encode(value) : value,
  );
  const total = encoded.reduce((sum, value) => sum + 4 + value.length, 0);
  const out = new Uint8Array(total);
  let offset = 0;
  for (const value of encoded) {
    writeUint32(out, offset, value.length);
    offset += 4;
    out.set(value, offset);
    offset += value.length;
  }
  return out;
}

async function signSSHData(key: UnlockedSSHKey, data: Uint8Array, flags: number) {
  if (key.type === "ssh-ed25519") {
    return {
      format: "ssh-ed25519",
      signature: bytesToBase64(ed25519.sign(data, key.seed)),
    };
  }

  const { format, hash } = rsaSignatureAlgorithm(flags);
  const cryptoKey = await crypto.subtle.importKey(
    "jwk",
    key.jwk,
    { name: "RSASSA-PKCS1-v1_5", hash },
    false,
    ["sign"],
  );
  const signature = await crypto.subtle.sign(
    { name: "RSASSA-PKCS1-v1_5" },
    cryptoKey,
    asBufferSource(data),
  );

  return {
    format,
    signature: bytesToBase64(new Uint8Array(signature)),
  };
}

function rsaSignatureAlgorithm(flags: number) {
  if ((flags & 4) !== 0) {
    return { format: "rsa-sha2-512", hash: "SHA-512" };
  }
  return { format: "rsa-sha2-256", hash: "SHA-256" };
}

async function deriveEncryptionKey(passphrase: string, salt: Uint8Array, iterations: number) {
  if (passphrase.length < 8) {
    throw new Error("Use a key storage passphrase of at least 8 characters");
  }

  const baseKey = await crypto.subtle.importKey(
    "raw",
    textEncoder.encode(passphrase),
    "PBKDF2",
    false,
    ["deriveKey"],
  );
  return crypto.subtle.deriveKey(
    {
      name: "PBKDF2",
      hash: "SHA-256",
      salt: asBufferSource(salt),
      iterations,
    },
    baseKey,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
}

function asBufferSource(bytes: Uint8Array): Uint8Array<ArrayBuffer> {
  const copy = new Uint8Array(bytes.length);
  copy.set(bytes);
  return copy;
}

function writeUint32(out: Uint8Array, offset: number, value: number) {
  out[offset] = (value >>> 24) & 0xff;
  out[offset + 1] = (value >>> 16) & 0xff;
  out[offset + 2] = (value >>> 8) & 0xff;
  out[offset + 3] = value & 0xff;
}

function pemBlockBase64(pem: string, label: string) {
  const pattern = new RegExp(
    `-----BEGIN ${label}-----([\\s\\S]+?)-----END ${label}-----`,
    "m",
  );
  const match = pem.match(pattern);
  if (!match?.[1]) {
    throw new Error(`Paste a valid ${label}`);
  }

  const base64 = match[1].replace(/\s+/g, "");
  if (!/^[A-Za-z0-9+/=]+$/.test(base64)) {
    throw new Error(`${label} contains invalid base64 data`);
  }
  return base64;
}

function toMPInt(bytes: Uint8Array) {
  const unsigned = unsignedBytes(bytes);
  if (unsigned.length === 0) {
    return unsigned;
  }
  if ((unsigned[0] & 0x80) === 0) {
    return unsigned;
  }
  const out = new Uint8Array(unsigned.length + 1);
  out.set(unsigned, 1);
  return out;
}

function unsignedBytes(bytes: Uint8Array) {
  let offset = 0;
  while (offset < bytes.length - 1 && bytes[offset] === 0) {
    offset++;
  }
  return bytes.slice(offset);
}

function bytesToBase64URL(bytes: Uint8Array) {
  return bytesToBase64(bytes).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function bytesToBigint(bytes: Uint8Array) {
  let value = 0n;
  for (const byte of unsignedBytes(bytes)) {
    value = (value << 8n) + BigInt(byte);
  }
  return value;
}

function bigintToUnsignedBytes(value: bigint) {
  if (value === 0n) {
    return new Uint8Array([0]);
  }
  const bytes: number[] = [];
  while (value > 0n) {
    bytes.unshift(Number(value & 0xffn));
    value >>= 8n;
  }
  return new Uint8Array(bytes);
}

function bytesEqual(a: Uint8Array, b: Uint8Array) {
  if (a.length !== b.length) {
    return false;
  }
  let result = 0;
  for (let i = 0; i < a.length; i++) {
    result |= a[i] ^ b[i];
  }
  return result === 0;
}

class SSHReader {
  private offset = 0;

  constructor(private readonly bytes: Uint8Array) {}

  readUint32() {
    if (this.offset + 4 > this.bytes.length) {
      throw new Error("Unexpected end of OpenSSH key");
    }
    const value =
      (this.bytes[this.offset] << 24) |
      (this.bytes[this.offset + 1] << 16) |
      (this.bytes[this.offset + 2] << 8) |
      this.bytes[this.offset + 3];
    this.offset += 4;
    return value >>> 0;
  }

  readBytes(length: number) {
    if (this.offset + length > this.bytes.length) {
      throw new Error("Unexpected end of OpenSSH key");
    }
    const value = this.bytes.slice(this.offset, this.offset + length);
    this.offset += length;
    return value;
  }

  readString() {
    return this.readBytes(this.readUint32());
  }

  readStringText() {
    return textDecoder.decode(this.readString());
  }
}

class DERReader {
  private offset = 0;

  constructor(private readonly bytes: Uint8Array) {}

  readConstructed(tag: number) {
    const actual = this.readByte();
    if (actual !== tag) {
      throw new Error("Invalid RSA private key");
    }
    return this.readBytes(this.readLength());
  }

  readInteger() {
    const tag = this.readByte();
    if (tag !== 0x02) {
      throw new Error("Invalid RSA private key");
    }
    return unsignedBytes(this.readBytes(this.readLength()));
  }

  private readLength() {
    const first = this.readByte();
    if ((first & 0x80) === 0) {
      return first;
    }

    const byteCount = first & 0x7f;
    if (byteCount === 0 || byteCount > 4) {
      throw new Error("Invalid RSA private key length");
    }

    let length = 0;
    for (let i = 0; i < byteCount; i++) {
      length = (length << 8) | this.readByte();
    }
    return length;
  }

  private readByte() {
    if (this.offset >= this.bytes.length) {
      throw new Error("Unexpected end of RSA private key");
    }
    return this.bytes[this.offset++];
  }

  private readBytes(length: number) {
    if (this.offset + length > this.bytes.length) {
      throw new Error("Unexpected end of RSA private key");
    }
    const value = this.bytes.slice(this.offset, this.offset + length);
    this.offset += length;
    return value;
  }
}
