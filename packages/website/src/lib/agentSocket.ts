"use client";

import { authenticatedWebSocketURL } from "@/lib/api";
import {
  base64ToBytes,
  publicKeyBlobBase64,
  signAgentRequest,
  type UnlockedSSHKey,
} from "@/lib/sshKey";

export function openAgentSocket(sessionId: string, sshKey: UnlockedSSHKey | null) {
  if (!sshKey) {
    return null;
  }

  const socket = new WebSocket(authenticatedWebSocketURL(`/api/sessions/${sessionId}/agent`));
  const publicKeyBlob = publicKeyBlobBase64(sshKey.publicKey);

  socket.addEventListener("open", () => {
    socket.send(
      JSON.stringify({
        type: "identities",
        keys: [
          {
            publicKey: sshKey.publicKey,
            comment: sshKey.comment,
          },
        ],
      }),
    );
  });

  socket.addEventListener("message", (event) => {
    void handleAgentMessage(socket, sshKey, publicKeyBlob, String(event.data));
  });

  return socket;
}

async function handleAgentMessage(
  socket: WebSocket,
  sshKey: UnlockedSSHKey,
  publicKeyBlob: string,
  payload: string,
) {
  const message = JSON.parse(payload) as {
    type: string;
    requestId?: string;
    publicKey?: string;
    data?: string;
    flags?: number;
  };

  if (message.type !== "sign" || !message.requestId || !message.data) {
    return;
  }

  if (message.publicKey !== publicKeyBlob) {
    socket.send(
      JSON.stringify({
        type: "failure",
        requestId: message.requestId,
        error: "requested key is not unlocked in this browser",
      }),
    );
    return;
  }

  try {
    const signature = await signAgentRequest(sshKey, base64ToBytes(message.data), message.flags ?? 0);
    socket.send(
      JSON.stringify({
        type: "signature",
        requestId: message.requestId,
        ...signature,
      }),
    );
  } catch (err) {
    socket.send(
      JSON.stringify({
        type: "failure",
        requestId: message.requestId,
        error: err instanceof Error ? err.message : "signing failed",
      }),
    );
  }
}
