export type AuthUser = {
  fingerprint: string;
  keyType: string;
  comment?: string;
};

export type SessionInfo = {
  id: string;
  name: string;
  kind: "terminal" | "gui";
  createdAt: string;
  lastActiveAt: string;
  width: number;
  height: number;
  display?: string;
  state: "starting" | "running" | "exited";
};

export type FeatureInfo = {
  e2ee: {
    enabled: boolean;
    required: boolean;
    cipher: string;
    keyExchange: string;
    compressionEnabled: boolean;
    compressionCodec: string;
    compressionMinBytes: number;
  };
  audio: {
    enabled: boolean;
    codec: string;
    sampleRate: number;
    channels: number;
    latencyMs: number;
  };
};

export class APIError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "APIError";
    this.status = status;
  }
}

const apiBase = (process.env.NEXT_PUBLIC_API_BASE ?? "").replace(/\/$/, "");
const wsBase = (process.env.NEXT_PUBLIC_WS_BASE ?? "").replace(/\/$/, "");
const tokenKey = "webshell.sessionToken";

export function getAuthToken() {
  if (typeof window === "undefined") {
    return null;
  }
  return window.sessionStorage.getItem(tokenKey);
}

export function setAuthToken(token: string | null) {
  if (typeof window === "undefined") {
    return;
  }
  if (token) {
    window.sessionStorage.setItem(tokenKey, token);
  } else {
    window.sessionStorage.removeItem(tokenKey);
  }
}

export function apiPath(path: string) {
  return `${apiBase}${path}`;
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  const isFormData = typeof FormData !== "undefined" && init.body instanceof FormData;
  if (init.body && !isFormData && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  const token = getAuthToken();
  if (token && !headers.has("Authorization")) {
    headers.set("Authorization", `Bearer ${token}`);
  }

  const response = await fetch(apiPath(path), {
    ...init,
    headers,
  });

  if (!response.ok) {
    let message = response.statusText;
    try {
      const payload = (await response.json()) as { error?: string };
      message = payload.error ?? message;
    } catch {
      // Keep the HTTP status text if the server did not return JSON.
    }
    throw new APIError(response.status, message);
  }

  if (response.status === 204) {
    return undefined as T;
  }

  return (await response.json()) as T;
}

export function websocketURL(path: string) {
  if (wsBase) {
    return `${wsBase}${path}`;
  }

  if (apiBase) {
    const url = new URL(apiBase);
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    return `${url.origin}${path}`;
  }

  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${window.location.host}${path}`;
}

export function authenticatedWebSocketURL(path: string) {
  const token = getAuthToken();
  const url = new URL(websocketURL(path));
  if (token) {
    url.searchParams.set("token", token);
  }
  return url.toString();
}

let featuresPromise: Promise<FeatureInfo> | null = null;

export function features() {
  if (!featuresPromise) {
    featuresPromise = api<FeatureInfo>("/api/features");
  }
  return featuresPromise;
}
