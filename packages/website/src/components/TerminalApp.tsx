"use client";

import { FormEvent, type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Check,
  ClipboardCopy,
  ClipboardPaste,
  KeyRound,
  LogOut,
  Minus,
  Monitor,
  PanelLeftClose,
  PanelLeftOpen,
  Pencil,
  Plus,
  SquareTerminal,
  Trash2,
  X,
} from "lucide-react";
import { GUIViewer } from "@/components/GUIViewer";
import { TerminalPane } from "@/components/TerminalPane";
import type { AuthUser, SessionInfo } from "@/lib/api";
import { api, APIError, getAuthToken, setAuthToken } from "@/lib/api";
import { authorizeGUIAudio } from "@/lib/guiAudio";
import {
  base64ToBytes,
  clearEncryptedKey,
  decryptStoredKey,
  encryptForStorage,
  hasStoredKey,
  loadEncryptedKey,
  parseSSHPrivateKey,
  saveEncryptedKey,
  signAuthChallenge,
  type UnlockedSSHKey,
} from "@/lib/sshKey";

type AuthStatus = "checking" | "anonymous" | "authenticated";
const guiViewerToolbarHeight = 44;

export function TerminalApp() {
  const [authStatus, setAuthStatus] = useState<AuthStatus>("checking");
  const [user, setUser] = useState<AuthUser | null>(null);
  const [sshKey, setSSHKey] = useState<UnlockedSSHKey | null>(null);
  const [sessions, setSessions] = useState<SessionInfo[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const [fontSize, setFontSize] = useState(14);
  const [reconnectToken, setReconnectToken] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [clipboardBusy, setClipboardBusy] = useState<"receive" | "send" | null>(null);
  const bootstrappedSessions = useRef(false);
  const workspaceRef = useRef<HTMLDivElement | null>(null);

  const activeSession = useMemo(
    () => sessions.find((session) => session.id === activeId) ?? sessions[0] ?? null,
    [activeId, sessions],
  );

  const refreshSessions = useCallback(async (ensureSession = false) => {
    const response = await api<{ sessions: SessionInfo[] }>("/api/sessions");
    if (ensureSession && response.sessions.length === 0) {
      const created = await api<{ session: SessionInfo }>("/api/sessions", {
        method: "POST",
        body: JSON.stringify({ kind: "terminal", name: "Terminal 1", cols: 100, rows: 32 }),
      });
      setSessions([created.session]);
      setActiveId(created.session.id);
      return;
    }

    setSessions(response.sessions);
    setActiveId((current) => {
      if (current && response.sessions.some((session) => session.id === current)) {
        return current;
      }
      return response.sessions[0]?.id ?? null;
    });
  }, []);

  useEffect(() => {
    async function boot() {
      if (!getAuthToken()) {
        setAuthStatus("anonymous");
        return;
      }

      try {
        const me = await api<{ authenticated: boolean; user: AuthUser }>("/api/me");
        setUser(me.authenticated ? me.user : null);
        setAuthStatus(me.authenticated ? "authenticated" : "anonymous");
        if (!me.authenticated) {
          setAuthToken(null);
        }
      } catch (err) {
        setAuthToken(null);
        setAuthStatus("anonymous");
        setError(err instanceof Error ? err.message : "Could not reach WebShell");
      }
    }

    void boot();
  }, []);

  useEffect(() => {
    if (authStatus !== "authenticated" || bootstrappedSessions.current) {
      return;
    }

    bootstrappedSessions.current = true;
    refreshSessions(true).catch((err) => {
      setError(err instanceof Error ? err.message : "Could not load sessions");
    });
  }, [authStatus, refreshSessions]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (!event.metaKey && !event.ctrlKey) {
        return;
      }

      if (event.key === "+" || event.key === "=") {
        event.preventDefault();
        setFontSize((size) => Math.min(size + 1, 22));
      }
      if (event.key === "-") {
        event.preventDefault();
        setFontSize((size) => Math.max(size - 1, 11));
      }
    };

    window.addEventListener("keydown", onKeyDown, { capture: true });
    return () => window.removeEventListener("keydown", onKeyDown, { capture: true });
  }, []);

  const createSession = async (kind: SessionInfo["kind"]) => {
    setError(null);
    let body: { kind: SessionInfo["kind"]; cols?: number; rows?: number; width?: number; height?: number };
    if (kind === "gui") {
      const rect = workspaceRef.current?.getBoundingClientRect();
      body = {
        kind,
        width: rect ? Math.max(640, Math.floor(rect.width)) : undefined,
        height: rect ? Math.max(480, Math.floor(rect.height - guiViewerToolbarHeight)) : undefined,
      };
    } else {
      body = { kind, cols: 100, rows: 32 };
    }
    const created = await api<{ session: SessionInfo }>("/api/sessions", {
      method: "POST",
      body: JSON.stringify(body),
    });
    setSessions((current) => [...current, created.session]);
    setActiveId(created.session.id);
    setSidebarOpen(false);
  };

  const renameSession = async (id: string, name: string) => {
    const updated = await api<{ session: SessionInfo }>(`/api/sessions/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ name }),
    });
    setSessions((current) =>
      current.map((session) => (session.id === id ? updated.session : session)),
    );
  };

  const closeSession = async (id: string) => {
    await api<void>(`/api/sessions/${id}`, { method: "DELETE" });
    removeSession(id);
  };

  const removeSession = useCallback((id: string) => {
    setSessions((current) => {
      const remaining = current.filter((session) => session.id !== id);
      setActiveId((active) => {
        if (active !== id) {
          return active;
        }
        return remaining[0]?.id ?? null;
      });
      return remaining;
    });
  }, []);

  const logout = async () => {
    await api("/api/logout", { method: "POST" });
    setAuthToken(null);
    setAuthStatus("anonymous");
    setUser(null);
    setSSHKey(null);
    setSessions([]);
    setActiveId(null);
    bootstrappedSessions.current = false;
  };

  const receiveClipboard = async () => {
    if (!activeSession || activeSession.kind !== "gui") {
      return;
    }
    setClipboardBusy("receive");
    setError(null);
    try {
      const response = await api<{ text: string }>(`/api/sessions/${activeSession.id}/clipboard`);
      await navigator.clipboard.writeText(response.text);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setClipboardBusy(null);
    }
  };

  const sendClipboard = async () => {
    if (!activeSession || activeSession.kind !== "gui") {
      return;
    }
    setClipboardBusy("send");
    setError(null);
    try {
      const payload = await readLocalClipboardForRemote();
      if (payload.kind === "files") {
        await api<{ ok: boolean }>(`/api/sessions/${activeSession.id}/clipboard/files`, {
          method: "PUT",
          body: payload.formData,
        });
      } else {
        await api<{ ok: boolean }>(`/api/sessions/${activeSession.id}/clipboard`, {
          method: "PUT",
          body: JSON.stringify({ text: payload.text }),
        });
      }
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setClipboardBusy(null);
    }
  };

  if (authStatus === "checking") {
    return <ShellFrame>Connecting</ShellFrame>;
  }

  if (authStatus === "anonymous") {
    return (
      <LoginView
        error={error}
        onAuthenticated={(nextUser, key) => {
          setUser(nextUser);
          setSSHKey(key);
          setAuthStatus("authenticated");
        }}
      />
    );
  }

  return (
    <main className="flex h-dvh w-screen overflow-hidden bg-neutral-950 text-neutral-100">
      <aside
        className={[
          "absolute inset-y-0 left-0 z-20 flex w-[min(82vw,20rem)] shrink-0 flex-col border-r border-neutral-800 bg-neutral-900 transition-transform duration-200 md:static md:w-72",
          sidebarOpen ? "translate-x-0" : "-translate-x-full md:translate-x-0",
        ].join(" ")}
      >
        <div className="flex h-14 shrink-0 items-center justify-between border-b border-neutral-800 px-3">
          <div className="flex min-w-0 items-center gap-2">
            <SquareTerminal className="h-5 w-5 shrink-0 text-emerald-400" aria-hidden="true" />
            <span className="truncate text-sm font-semibold tracking-normal">WebShell</span>
          </div>
          <button
            type="button"
            onClick={() => setSidebarOpen(false)}
            title="Hide sessions"
            className="grid h-9 w-9 place-items-center rounded-md text-neutral-300 hover:bg-neutral-800 md:hidden"
          >
            <PanelLeftClose className="h-4 w-4" aria-hidden="true" />
            <span className="sr-only">Hide sessions</span>
          </button>
        </div>

        <div className="flex min-h-0 flex-1 flex-col">
          <div className="flex h-12 shrink-0 items-center justify-between px-3">
            <span className="text-xs font-medium uppercase text-neutral-500">Sessions</span>
            <div className="flex items-center gap-1">
              <button
                type="button"
                onClick={() =>
                  void createSession("terminal").catch((err) => setError(errorMessage(err)))
                }
                title="New terminal"
                className="grid h-8 w-8 place-items-center rounded-md text-neutral-200 hover:bg-neutral-800"
              >
                <SquareTerminal className="h-4 w-4" aria-hidden="true" />
                <span className="sr-only">New terminal</span>
              </button>
              <button
                type="button"
                onClick={() => {
                  authorizeGUIAudio();
                  void createSession("gui").catch((err) => setError(errorMessage(err)));
                }}
                title="New desktop"
                className="grid h-8 w-8 place-items-center rounded-md text-neutral-200 hover:bg-neutral-800"
              >
                <Monitor className="h-4 w-4" aria-hidden="true" />
                <span className="sr-only">New desktop</span>
              </button>
            </div>
          </div>

          <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-3">
            {sessions.map((session) => (
              <SessionRow
                key={session.id}
                session={session}
                active={session.id === activeSession?.id}
                onSelect={() => {
                  if (session.kind === "gui") {
                    authorizeGUIAudio();
                  }
                  setActiveId(session.id);
                  setSidebarOpen(false);
                }}
                onRename={(name) =>
                  renameSession(session.id, name).catch((err) => setError(errorMessage(err)))
                }
                onClose={() => closeSession(session.id).catch((err) => setError(errorMessage(err)))}
              />
            ))}
          </div>
        </div>

        <div className="shrink-0 border-t border-neutral-800 p-3">
          <div className="flex items-center gap-2">
            <KeyRound className="h-8 w-8 shrink-0 rounded-full bg-neutral-800 p-1.5 text-neutral-300" />
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm font-medium text-neutral-100">
                {user?.comment || user?.fingerprint}
              </div>
              <div className="truncate text-xs text-neutral-500">{user?.keyType}</div>
            </div>
            <button
              type="button"
              onClick={() => void logout().catch((err) => setError(errorMessage(err)))}
              title="Log out"
              className="grid h-8 w-8 place-items-center rounded-md text-neutral-300 hover:bg-neutral-800 hover:text-neutral-50"
            >
              <LogOut className="h-4 w-4" aria-hidden="true" />
              <span className="sr-only">Log out</span>
            </button>
          </div>
        </div>
      </aside>

      {sidebarOpen ? (
        <button
          type="button"
          aria-label="Close sessions"
          className="absolute inset-0 z-10 bg-black/40 md:hidden"
          onClick={() => setSidebarOpen(false)}
        />
      ) : null}

      <section className="flex min-w-0 flex-1 flex-col">
        <div className="flex h-14 shrink-0 items-center justify-between border-b border-neutral-800 bg-neutral-900 px-3 md:px-4">
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => setSidebarOpen((open) => !open)}
              title={sidebarOpen ? "Hide sessions" : "Show sessions"}
              className="grid h-9 w-9 place-items-center rounded-md text-neutral-300 hover:bg-neutral-800"
            >
              {sidebarOpen ? (
                <PanelLeftClose className="h-4 w-4" aria-hidden="true" />
              ) : (
                <PanelLeftOpen className="h-4 w-4" aria-hidden="true" />
              )}
              <span className="sr-only">{sidebarOpen ? "Hide sessions" : "Show sessions"}</span>
            </button>
            <div className="min-w-0">
              <div className="truncate text-sm font-medium text-neutral-100">
                {activeSession ? displaySessionName(activeSession) : "No session"}
              </div>
              <div className="truncate text-xs text-neutral-500">
                {activeSession
                  ? `${sessionKindLabel(activeSession.kind)} · ${activeSession.width}x${activeSession.height}`
                  : "idle"}
              </div>
            </div>
          </div>

          {activeSession?.kind === "gui" ? (
            <div className="flex items-center gap-1">
              <button
                type="button"
                onClick={() => void receiveClipboard()}
                disabled={clipboardBusy !== null}
                title="Receive clipboard"
                className="grid h-9 w-9 place-items-center rounded-md text-neutral-300 hover:bg-neutral-800 hover:text-neutral-50 disabled:cursor-not-allowed disabled:opacity-50"
              >
                <ClipboardPaste className="h-4 w-4" aria-hidden="true" />
                <span className="sr-only">Receive clipboard</span>
              </button>
              <button
                type="button"
                onClick={() => void sendClipboard()}
                disabled={clipboardBusy !== null}
                title="Send clipboard"
                className="grid h-9 w-9 place-items-center rounded-md text-neutral-300 hover:bg-neutral-800 hover:text-neutral-50 disabled:cursor-not-allowed disabled:opacity-50"
              >
                <ClipboardCopy className="h-4 w-4" aria-hidden="true" />
                <span className="sr-only">Send clipboard</span>
              </button>
            </div>
          ) : activeSession?.kind === "terminal" ? (
            <div className="flex items-center gap-1">
              <button
                type="button"
                onClick={() => setFontSize((size) => Math.max(size - 1, 11))}
                title="Smaller text"
                className="grid h-9 w-9 place-items-center rounded-md text-neutral-300 hover:bg-neutral-800"
              >
                <Minus className="h-4 w-4" aria-hidden="true" />
                <span className="sr-only">Smaller text</span>
              </button>
              <span className="w-8 text-center text-xs tabular-nums text-neutral-500">{fontSize}</span>
              <button
                type="button"
                onClick={() => setFontSize((size) => Math.min(size + 1, 22))}
                title="Larger text"
                className="grid h-9 w-9 place-items-center rounded-md text-neutral-300 hover:bg-neutral-800"
              >
                <Plus className="h-4 w-4" aria-hidden="true" />
                <span className="sr-only">Larger text</span>
              </button>
            </div>
          ) : null}
        </div>

        {error ? (
          <div className="flex shrink-0 items-center justify-between border-b border-amber-500/30 bg-amber-500/10 px-3 py-2 text-sm text-amber-100">
            <span className="min-w-0 truncate">{error}</span>
            <button
              type="button"
              onClick={() => setError(null)}
              title="Dismiss"
              className="grid h-7 w-7 shrink-0 place-items-center rounded-md hover:bg-amber-500/20"
            >
              <X className="h-4 w-4" aria-hidden="true" />
              <span className="sr-only">Dismiss</span>
            </button>
          </div>
        ) : null}

        <div ref={workspaceRef} className="min-h-0 flex-1">
          {activeSession?.kind === "gui" ? (
            <GUIViewer
              session={activeSession}
              reconnectToken={reconnectToken}
              sshKey={sshKey}
              onReconnect={() => setReconnectToken((value) => value + 1)}
              onSessionExit={removeSession}
              onError={setError}
            />
          ) : (
            <TerminalPane
              session={activeSession}
              fontSize={fontSize}
              reconnectToken={reconnectToken}
              sshKey={sshKey}
              onReconnect={() => setReconnectToken((value) => value + 1)}
              onSessionExit={removeSession}
            />
          )}
        </div>
      </section>
    </main>
  );
}

function LoginView({
  error,
  onAuthenticated,
}: {
  error: string | null;
  onAuthenticated: (user: AuthUser, key: UnlockedSSHKey) => void;
}) {
  const [stored, setStored] = useState(hasStoredKey);
  const [importing, setImporting] = useState(!stored);
  const [privateKey, setPrivateKey] = useState("");
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [localError, setLocalError] = useState<string | null>(null);

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setBusy(true);
    setLocalError(null);
    setAuthToken(null);

    try {
      let key: UnlockedSSHKey;
      let encryptedKey: Awaited<ReturnType<typeof encryptForStorage>> | null = null;

      if (importing) {
        key = parseSSHPrivateKey(privateKey);
        encryptedKey = await encryptForStorage(key, passphrase);
      } else {
        const storedKey = loadEncryptedKey();
        if (!storedKey) {
          throw new Error("No SSH key is stored in this browser");
        }
        key = await decryptStoredKey(storedKey, passphrase);
      }

      const challenge = await api<{
        challengeId: string;
        nonce: string;
        expiresAt: string;
      }>("/api/auth/challenge", { method: "POST" });

      const signature = await signAuthChallenge(key, base64ToBytes(challenge.nonce));
      const response = await api<{
        token: string;
        authenticated: boolean;
        user: AuthUser;
      }>("/api/auth/verify", {
        method: "POST",
        body: JSON.stringify({
          challengeId: challenge.challengeId,
          publicKey: key.publicKey,
          signature,
        }),
      });

      if (encryptedKey) {
        saveEncryptedKey(encryptedKey);
        setStored(true);
        setImporting(false);
      }

      setAuthToken(response.token);
      onAuthenticated(response.user, key);
    } catch (err) {
      setLocalError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const clearKey = () => {
    clearEncryptedKey();
    setStored(false);
    setImporting(true);
    setPrivateKey("");
    setPassphrase("");
  };

  return (
    <ShellFrame>
      <div className="w-full max-w-md border border-neutral-800 bg-neutral-900 p-5 shadow-2xl shadow-black/30">
        <div className="mb-5 flex items-center gap-2">
          <SquareTerminal className="h-5 w-5 text-emerald-400" aria-hidden="true" />
          <h1 className="text-base font-semibold text-neutral-100">WebShell</h1>
        </div>

        <form onSubmit={onSubmit}>
          {importing ? (
            <>
              <label className="block text-xs font-medium text-neutral-400" htmlFor="privateKey">
                SSH private key
              </label>
              <textarea
                id="privateKey"
                value={privateKey}
                onChange={(event) => setPrivateKey(event.target.value)}
                spellCheck={false}
                className="mt-1 h-44 w-full resize-none rounded-md border border-neutral-700 bg-neutral-950 px-3 py-2 font-mono text-xs text-neutral-100 outline-none focus:border-emerald-500"
              />
            </>
          ) : (
            <div className="flex items-center gap-3 rounded-md border border-neutral-800 bg-neutral-950 px-3 py-3">
              <KeyRound className="h-5 w-5 shrink-0 text-emerald-400" aria-hidden="true" />
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm text-neutral-100">
                  {loadEncryptedKey()?.comment || loadEncryptedKey()?.publicKey || "Stored key"}
                </div>
                <div className="truncate text-xs text-neutral-500">LocalStorage</div>
              </div>
              <button
                type="button"
                onClick={() => setImporting(true)}
                className="rounded-md px-2 py-1 text-xs text-neutral-300 hover:bg-neutral-800"
              >
                Replace
              </button>
            </div>
          )}

          <label className="mt-4 block text-xs font-medium text-neutral-400" htmlFor="passphrase">
            Key storage passphrase
          </label>
          <input
            id="passphrase"
            type="password"
            value={passphrase}
            onChange={(event) => setPassphrase(event.target.value)}
            autoComplete="current-password"
            className="mt-1 h-10 w-full rounded-md border border-neutral-700 bg-neutral-950 px-3 text-sm text-neutral-100 outline-none focus:border-emerald-500"
          />

          <button
            type="submit"
            disabled={busy}
            className="mt-4 h-10 w-full rounded-md bg-emerald-500 px-3 text-sm font-medium text-neutral-950 hover:bg-emerald-400 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {busy ? "Signing in" : "Sign in with SSH key"}
          </button>
        </form>

        {stored ? (
          <button
            type="button"
            onClick={clearKey}
            className="mt-3 h-9 w-full rounded-md border border-neutral-800 px-3 text-sm text-neutral-300 hover:bg-neutral-800"
          >
            Clear stored key
          </button>
        ) : null}

        {error || localError ? (
          <p className="mt-4 text-sm text-amber-300">{localError ?? error}</p>
        ) : null}
      </div>
    </ShellFrame>
  );
}

function ShellFrame({ children }: { children: ReactNode }) {
  return (
    <main className="grid h-dvh w-screen place-items-center bg-neutral-950 px-4 text-neutral-100">
      {typeof children === "string" ? (
        <div className="font-mono text-sm text-neutral-500">{children}</div>
      ) : (
        children
      )}
    </main>
  );
}

function SessionRow({
  session,
  active,
  onSelect,
  onRename,
  onClose,
}: {
  session: SessionInfo;
  active: boolean;
  onSelect: () => void;
  onRename: (name: string) => void;
  onClose: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(displaySessionName(session));

  useEffect(() => {
    setName(displaySessionName(session));
  }, [session]);

  const save = () => {
    const nextName = name.trim();
    if (nextName && nextName !== displaySessionName(session)) {
      onRename(nextName);
    }
    setEditing(false);
  };
  const KindIcon = session.kind === "gui" ? Monitor : SquareTerminal;

  return (
    <div
      className={[
        "mb-1 grid min-h-12 grid-cols-[1fr_auto_auto] items-center gap-1 rounded-md border px-2 py-1.5",
        active
          ? "border-emerald-500/50 bg-emerald-500/10"
          : "border-transparent hover:border-neutral-800 hover:bg-neutral-800/70",
      ].join(" ")}
    >
      {editing ? (
        <input
          value={name}
          onChange={(event) => setName(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              save();
            }
            if (event.key === "Escape") {
              setName(displaySessionName(session));
              setEditing(false);
            }
          }}
          autoFocus
          className="min-w-0 rounded-md border border-neutral-700 bg-neutral-950 px-2 py-1 text-sm text-neutral-100 outline-none focus:border-emerald-500"
        />
      ) : (
        <button type="button" onClick={onSelect} className="min-w-0 text-left">
          <div className="flex min-w-0 items-center gap-2">
            <KindIcon
              className={[
                "h-4 w-4 shrink-0",
                session.kind === "gui" ? "text-sky-300" : "text-emerald-400",
              ].join(" ")}
              aria-hidden="true"
            />
            <div className="min-w-0">
              <div className="truncate text-sm font-medium text-neutral-100">
                {displaySessionName(session)}
              </div>
              <div className="truncate text-xs text-neutral-500">
                {sessionKindLabel(session.kind)} · {session.state} · {session.width}x{session.height} ·{" "}
                {timeLabel(session.lastActiveAt)}
              </div>
            </div>
          </div>
        </button>
      )}

      {editing ? (
        <button
          type="button"
          title="Save name"
          onClick={save}
          className="grid h-8 w-8 place-items-center rounded-md text-neutral-300 hover:bg-neutral-700"
        >
          <Check className="h-4 w-4" aria-hidden="true" />
          <span className="sr-only">Save name</span>
        </button>
      ) : (
        <button
          type="button"
          title="Rename"
          onClick={() => setEditing(true)}
          className="grid h-8 w-8 place-items-center rounded-md text-neutral-400 hover:bg-neutral-700 hover:text-neutral-100"
        >
          <Pencil className="h-4 w-4" aria-hidden="true" />
          <span className="sr-only">Rename</span>
        </button>
      )}

      <button
        type="button"
        title="Close session"
        onClick={onClose}
        className="grid h-8 w-8 place-items-center rounded-md text-neutral-400 hover:bg-red-500/15 hover:text-red-300"
      >
        <Trash2 className="h-4 w-4" aria-hidden="true" />
        <span className="sr-only">Close session</span>
      </button>
    </div>
  );
}

function timeLabel(value: string) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "active";
  }
  return new Intl.DateTimeFormat(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  }).format(date);
}

function displaySessionName(session: SessionInfo) {
  if (session.kind === "gui") {
    return session.name.replace(/^GUI(\s+\d+)$/i, "Desktop$1");
  }
  return session.name;
}

function sessionKindLabel(kind: SessionInfo["kind"]) {
  return kind === "gui" ? "Desktop" : "Terminal";
}

function errorMessage(err: unknown) {
  if (err instanceof APIError) {
    return err.message;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return "Something went wrong";
}

type LocalClipboardPayload =
  | { kind: "text"; text: string }
  | { kind: "files"; formData: FormData };

async function readLocalClipboardForRemote(): Promise<LocalClipboardPayload> {
  const clipboard = navigator.clipboard as Clipboard & {
    read?: () => Promise<ClipboardItem[]>;
  };

  if (clipboard.read) {
    const items = await clipboard.read();
    const formData = new FormData();
    let fileCount = 0;
    let sawLocalFileURI = false;

    for (const item of items) {
      for (const type of item.types) {
        if (isClipboardTextType(type)) {
          sawLocalFileURI ||= await clipboardItemContainsLocalFileURI(item, type);
          continue;
        }

        let blob: Blob;
        try {
          blob = await item.getType(type);
        } catch {
          continue;
        }
        if (blob.size === 0) {
          continue;
        }

        fileCount += 1;
        const name = clipboardBlobName(blob, type, fileCount);
        formData.append("files", blob, name);
        formData.append("paths", name);
      }
    }

    if (fileCount > 0) {
      return { kind: "files", formData };
    }
    if (sawLocalFileURI) {
      throw new Error(
        "The browser exposed copied file paths, but not file contents. Copy from a source that grants clipboard file contents, or use a file upload flow.",
      );
    }
  }

  const text = await navigator.clipboard.readText();
  if (isLocalFileURIList(text)) {
    throw new Error(
      "The browser exposed copied file paths, but not file contents. Copy from a source that grants clipboard file contents, or use a file upload flow.",
    );
  }
  return { kind: "text", text };
}

function isClipboardTextType(type: string) {
  return type === "text/plain" || type === "text/html" || type === "text/uri-list";
}

async function clipboardItemContainsLocalFileURI(item: ClipboardItem, type: string) {
  try {
    const blob = await item.getType(type);
    const text = await blob.text();
    return isLocalFileURIList(text);
  } catch {
    return false;
  }
}

function isLocalFileURIList(text: string) {
  const lines = text
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line && !line.startsWith("#"));
  return lines.length > 0 && lines.every((line) => line.startsWith("file://"));
}

function clipboardBlobName(blob: Blob, type: string, index: number) {
  if (typeof File !== "undefined" && blob instanceof File && blob.name) {
    return blob.name;
  }
  return `clipboard-file-${index}${clipboardExtension(type || blob.type)}`;
}

function clipboardExtension(type: string) {
  switch (type) {
    case "image/png":
      return ".png";
    case "image/jpeg":
      return ".jpg";
    case "image/gif":
      return ".gif";
    case "image/webp":
      return ".webp";
    case "application/pdf":
      return ".pdf";
    default:
      return "";
  }
}
