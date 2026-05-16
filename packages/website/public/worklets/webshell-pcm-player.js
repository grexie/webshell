class WebShellPCMPlayer extends AudioWorkletProcessor {
  constructor() {
    super();
    this.queue = [];
    this.queuedFrames = 0;
    this.readFrame = 0;
    this.sourceSampleRate = 48000;
    this.sourceChannels = 2;
    this.targetLatencySeconds = 0.35;
    this.maxLatencySeconds = 0.9;
    this.rebufferSeconds = 0.12;
    this.primed = false;
    this.closed = false;

    this.port.onmessage = (event) => {
      const message = event.data || {};
      if (message.type === "close") {
        this.closed = true;
        this.queue = [];
        this.queuedFrames = 0;
        this.readFrame = 0;
        return;
      }
      if (message.type !== "audio" || !message.data || this.closed) {
        return;
      }

      this.sourceSampleRate = Math.max(8000, message.sampleRate || 48000);
      this.sourceChannels = Math.max(1, Math.min(2, message.channels || 2));
      this.targetLatencySeconds = message.targetLatencySeconds || this.targetLatencySeconds;
      this.maxLatencySeconds = message.maxLatencySeconds || this.maxLatencySeconds;
      this.rebufferSeconds = message.rebufferSeconds || this.rebufferSeconds;

      const samples = new Int16Array(message.data);
      this.queue.push(samples);
      this.queuedFrames += Math.floor(samples.length / this.sourceChannels);
      this.trimQueueTo(this.sourceSampleRate * this.targetLatencySeconds);
    };
  }

  process(_inputs, outputs) {
    const output = outputs[0];
    if (!output || output.length === 0) {
      return !this.closed;
    }

    const left = output[0];
    const right = output[1] || left;
    left.fill(0);
    if (right !== left) {
      right.fill(0);
    }
    if (this.closed) {
      return false;
    }

    const minFrames = Math.floor(this.sourceSampleRate * this.targetLatencySeconds);
    if (!this.primed) {
      if (this.availableFrames() < minFrames) {
        return true;
      }
      this.trimQueueTo(this.sourceSampleRate * this.targetLatencySeconds);
      this.primed = true;
    }

    const ratio = this.sourceSampleRate / sampleRate;
    for (let i = 0; i < left.length; i++) {
      const sample = this.sampleAt(this.readFrame);
      if (!sample) {
        this.primed = false;
        break;
      }

      left[i] = sample.left;
      right[i] = sample.right;
      this.readFrame += ratio;
      this.dropFullyReadChunks();
    }

    if (this.availableFrames() < this.sourceSampleRate * this.rebufferSeconds) {
      this.primed = false;
    }

    return true;
  }

  sampleAt(frame) {
    let offset = Math.floor(frame);
    for (const chunk of this.queue) {
      const frames = Math.floor(chunk.length / this.sourceChannels);
      if (offset < frames) {
        const sampleOffset = offset * this.sourceChannels;
        const left = this.int16ToFloat(chunk[sampleOffset] || 0);
        const right = this.int16ToFloat(chunk[sampleOffset + (this.sourceChannels > 1 ? 1 : 0)] || 0);
        return { left, right };
      }
      offset -= frames;
    }
    return null;
  }

  trimQueueTo(frames) {
    const maxFrames = Math.floor(this.sourceSampleRate * this.maxLatencySeconds);
    const keepFrames = Math.floor(frames);
    while (this.availableFrames() > maxFrames && this.queue.length > 1) {
      this.dropOldestChunk();
    }
    while (this.availableFrames() > keepFrames && this.queue.length > 1) {
      this.dropOldestChunk();
    }
  }

  dropOldestChunk() {
    const dropped = this.queue.shift();
    if (!dropped) {
      return;
    }
    const frames = Math.floor(dropped.length / this.sourceChannels);
    this.queuedFrames -= frames;
    this.readFrame = Math.max(0, this.readFrame - frames);
  }

  dropFullyReadChunks() {
    while (this.queue.length > 0) {
      const frames = Math.floor(this.queue[0].length / this.sourceChannels);
      if (this.readFrame < frames) {
        return;
      }
      this.queue.shift();
      this.queuedFrames -= frames;
      this.readFrame -= frames;
    }
  }

  availableFrames() {
    return Math.max(0, this.queuedFrames - Math.floor(this.readFrame));
  }

  int16ToFloat(value) {
    return value < 0 ? value / 32768 : value / 32767;
  }
}

registerProcessor("webshell-pcm-player", WebShellPCMPlayer);
