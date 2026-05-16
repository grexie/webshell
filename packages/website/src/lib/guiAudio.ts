"use client";

export type GUIAudioPlayer = {
  pushPCM(data: ArrayBuffer, sampleRate: number, channels: number): void;
  resume(): void;
  close(): void;
};

const preferredSampleRate = 48000;
const targetLatencySeconds = 0.35;
const maxLatencySeconds = 0.9;
const rebufferSeconds = 0.12;
const workletURL = "/worklets/webshell-pcm-player.js?v=20260516-audio-350ms";

let audioContext: AudioContext | null = null;
let workletLoaded: Promise<void> | null = null;

export function authorizeGUIAudio() {
  if (typeof window === "undefined") {
    return;
  }

  void resumeGUIAudio();
}

export async function createGUIAudioPlayer(): Promise<GUIAudioPlayer> {
  const context = getAudioContext();
  await resumeGUIAudio();

  if (context.audioWorklet) {
    try {
      await loadWorklet(context);
      const node = new AudioWorkletNode(context, "webshell-pcm-player", {
        numberOfInputs: 0,
        numberOfOutputs: 1,
        outputChannelCount: [2],
      });
      node.connect(context.destination);
      return {
        pushPCM(data, sampleRate, channels) {
          node.port.postMessage(
            {
              type: "audio",
              data,
              sampleRate,
              channels,
              targetLatencySeconds,
              maxLatencySeconds,
              rebufferSeconds,
            },
            [data],
          );
        },
        resume() {
          void resumeGUIAudio();
        },
        close() {
          node.port.postMessage({ type: "close" });
          node.disconnect();
        },
      };
    } catch {
      // Fall through to ScriptProcessor. Safari and older embedded browsers can
      // expose AudioWorklet but reject addModule for static-exported assets.
    }
  }

  return createScriptProcessorPlayer(context);
}

export function resumeGUIAudio() {
  const context = getAudioContext();
  if (context.state !== "running") {
    void context.resume().catch(() => undefined);
  }

  const source = context.createBufferSource();
  source.buffer = context.createBuffer(1, 1, context.sampleRate);
  const gain = context.createGain();
  gain.gain.value = 0;
  source.connect(gain);
  gain.connect(context.destination);
  source.start();
  source.stop(context.currentTime + 0.01);
}

function getAudioContext() {
  if (audioContext) {
    return audioContext;
  }

  const webAudioWindow = window as Window & { webkitAudioContext?: typeof AudioContext };
  const AudioContextConstructor = window.AudioContext ?? webAudioWindow.webkitAudioContext;
  if (!AudioContextConstructor) {
    throw new Error("Web Audio is not supported by this browser");
  }

  try {
    audioContext = new AudioContextConstructor({
      latencyHint: "interactive",
      sampleRate: preferredSampleRate,
    });
  } catch {
    audioContext = new AudioContextConstructor({ latencyHint: "interactive" });
  }
  return audioContext;
}

function loadWorklet(context: AudioContext) {
  workletLoaded ??= context.audioWorklet.addModule(workletURL);
  return workletLoaded;
}

function createScriptProcessorPlayer(context: AudioContext): GUIAudioPlayer {
  const processor = context.createScriptProcessor(2048, 0, 2);
  const queue: Int16Array[] = [];
  let queuedFrames = 0;
  let sourceChannels = 2;
  let sourceSampleRate = preferredSampleRate;
  let readFrame = 0;
  let primed = false;
  let closed = false;

  processor.onaudioprocess = (event) => {
    const output = event.outputBuffer;
    const left = output.getChannelData(0);
    const right = output.numberOfChannels > 1 ? output.getChannelData(1) : left;
    left.fill(0);
    if (right !== left) {
      right.fill(0);
    }
    if (closed) {
      return;
    }

    const minFrames = Math.floor(sourceSampleRate * targetLatencySeconds);
    if (!primed) {
      if (availableFrames() < minFrames) {
        return;
      }
      trimQueueTo(sourceSampleRate * targetLatencySeconds);
      primed = true;
    }

    const ratio = sourceSampleRate / context.sampleRate;
    for (let i = 0; i < output.length; i++) {
      const sample = sampleAt(queue, readFrame, sourceChannels);
      if (!sample) {
        primed = false;
        break;
      }
      left[i] = sample.left;
      right[i] = sample.right;
      readFrame += ratio;
      dropFullyReadChunks();
    }

    if (availableFrames() < sourceSampleRate * rebufferSeconds) {
      primed = false;
    }
  };

  processor.connect(context.destination);

  const trimQueueTo = (frames: number) => {
    const maxFrames = Math.floor(sourceSampleRate * maxLatencySeconds);
    const keepFrames = Math.floor(frames);
    while (availableFrames() > maxFrames && queue.length > 1) {
      const dropped = queue.shift()!;
      queuedFrames -= Math.floor(dropped.length / sourceChannels);
      readFrame = Math.max(0, readFrame - Math.floor(dropped.length / sourceChannels));
    }
    while (availableFrames() > keepFrames && queue.length > 1) {
      const dropped = queue.shift()!;
      queuedFrames -= Math.floor(dropped.length / sourceChannels);
      readFrame = Math.max(0, readFrame - Math.floor(dropped.length / sourceChannels));
    }
  };

  return {
    pushPCM(data, sampleRate, channels) {
      if (closed || data.byteLength === 0) {
        return;
      }
      sourceSampleRate = sampleRate || preferredSampleRate;
      sourceChannels = Math.max(1, Math.min(2, channels || 2));
      const samples = new Int16Array(data);
      queue.push(samples);
      queuedFrames += Math.floor(samples.length / sourceChannels);
      trimQueueTo(sourceSampleRate * targetLatencySeconds);
    },
    resume() {
      void resumeGUIAudio();
    },
    close() {
      closed = true;
      queue.length = 0;
      processor.disconnect();
    },
  };

  function availableFrames() {
    return Math.max(0, queuedFrames - Math.floor(readFrame));
  }

  function dropFullyReadChunks() {
    while (queue.length > 0) {
      const frames = Math.floor(queue[0].length / sourceChannels);
      if (readFrame < frames) {
        return;
      }
      queue.shift();
      queuedFrames -= frames;
      readFrame -= frames;
    }
  }
}

function sampleAt(queue: Int16Array[], frame: number, channels: number) {
  let offset = Math.floor(frame);
  for (const chunk of queue) {
    const frames = Math.floor(chunk.length / channels);
    if (offset < frames) {
      const sampleOffset = offset * channels;
      const left = int16ToFloat(chunk[sampleOffset] ?? 0);
      const right = int16ToFloat(chunk[sampleOffset + (channels > 1 ? 1 : 0)] ?? 0);
      return { left, right };
    }
    offset -= frames;
  }
  return null;
}

function int16ToFloat(value: number) {
  return value < 0 ? value / 32768 : value / 32767;
}
