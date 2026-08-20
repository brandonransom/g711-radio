# G.711 Radio

A Go WebRTC server built with Pion. It reads a local JSON config file, listens on one UDP port per configured stream, extracts a 160-byte G.711 audio frame from each packet, and broadcasts each stream to browser clients over WebRTC. Optionally transcribes audio using a locally-installed whisper.cpp instance.

## Prerequisites

- **Go 1.24+**
- *(Optional)* **whisper.cpp** for server-side transcription — see [Transcription](#transcription) below

## Run it

```bash
go mod tidy
go run .
```

Edit the gitignored `config.local.json`, send your UDP audio to the configured ports, then open `http://localhost:<httpPort>`.

## Config

`config.local.json` contains the HTTP port, an optional whisper block, and a hierarchical `regions` map:

```json
{
  "httpPort": 8080,
  "whisper": {
    "modelPath": "C:\\path\\to\\ggml-medium.bin",
    "workers": 3,
    "vadThreshold": 0.02,
    "silenceMs": 600,
    "minClipMs": 300,
    "maxClipMs": 30000
  },
  "regions": {
    "California": {
      "Plumas NF": [
        { "streamName": "Admin Net", "udpPort": 51110 },
        { "streamName": "Forest Net", "udpPort": 51160 }
      ],
      "Tahoe NF": [
        { "streamName": "Fire East Net", "udpPort": 51790 }
      ]
    },
    "Oregon": {
      "Umatilla NF": [
        { "streamName": "Pomeroy Net", "udpPort": 15142 }
      ]
    }
  }
}
```

The `whisper` block is optional. Omit it entirely to run without transcription.

## Transcription

Transcription uses [whisper.cpp](https://github.com/ggerganov/whisper.cpp) running as a local subprocess. It must be installed separately — it is **not** managed by `go mod tidy`.

### Install whisper.cpp on Windows

1. Clone and build whisper.cpp:
   ```powershell
   git clone https://github.com/ggerganov/whisper.cpp
   cd whisper.cpp
   cmake -B build
   cmake --build build --config Release
   ```
2. Download a model (medium recommended for radio audio):
   ```powershell
   # From the whisper.cpp directory:
   .\models\download-ggml-model.cmd medium
   # Model will be at: models\ggml-medium.bin
   ```
3. Ensure `whisper-cli.exe` is on your PATH, or add the build output directory:
   ```powershell
   $env:PATH += ";C:\path\to\whisper.cpp\build\bin\Release"
   ```
4. Set `modelPath` in `config.local.json` to the full path of `ggml-medium.bin`.

### Install whisper.cpp on Linux

1. Install build dependencies:
   ```bash
   # Ubuntu/Debian
   sudo apt-get install -y build-essential cmake git

   # RHEL/Fedora
   sudo dnf install -y gcc gcc-c++ cmake git
   ```
2. Clone and build whisper.cpp:
   ```bash
   git clone https://github.com/ggerganov/whisper.cpp
   cd whisper.cpp
   cmake -B build
   cmake --build build --config Release -j$(nproc)
   ```
3. Download a model:
   ```bash
   bash models/download-ggml-model.sh medium
   # Model will be at: models/ggml-medium.bin
   ```
4. Install the binary to your PATH:
   ```bash
   sudo cp build/bin/whisper-cli /usr/local/bin/
   ```
5. Set `modelPath` in `config.local.json` to the full path of `ggml-medium.bin`.

#### Optional: GPU acceleration on Linux (NVIDIA)

If your server has a CUDA-capable GPU, build with CUDA support for significantly faster transcription:

```bash
cmake -B build -DGGML_CUDA=ON
cmake --build build --config Release -j$(nproc)
```

Requires CUDA toolkit (`nvidia-cuda-toolkit`) to be installed.

### How transcription works

- Each stream runs energy-based **Voice Activity Detection (VAD)** on the incoming audio
- When a transmission is detected, audio is buffered until silence holds for `silenceMs`
- The clip is submitted to a worker pool that calls `whisper-cli` as a subprocess
- Transcripts are broadcast to connected browsers via **Server-Sent Events** at `/transcripts`
- The individual stream page displays a live scrollable transcript panel

### Model selection

| Model | Size | Notes |
|-------|------|-------|
| `tiny` | 75 MB | Fast, low accuracy — not recommended for radio |
| `base` | 142 MB | Reasonable for clean speech |
| `small` | 466 MB | Good balance |
| `medium` | 1.5 GB | **Recommended** for radio audio quality |
| `large-v3` | 3 GB | Best accuracy, slowest |

## Notes

- The server assumes 8 kHz mono G.711 PCMU frames with a 12-byte transport header and 160 audio bytes per packet (20 ms).
- The client loads streams from `/streams`, renders them as a hierarchical region → group → stream accordion, and opens each feed in its own dedicated tab.
- The client uses non-trickle ICE and is intended for local or LAN use. For internet-facing deployments, add STUN/TURN configuration.
- Transcription requires Chrome or Edge on the client (SSE is supported in all modern browsers; the panel displays regardless).

## Test with GStreamer

An example sender lives at `examples/gst-launch-pcmu-sine.sh`. It generates an 8 kHz mono sine wave, encodes it as PCMU, packetizes it as RTP, and sends it to `127.0.0.1:2250`.

## Debugging with pcapng

```bash
go run ./cmd/replay-pcap -pcap g711.pcapng -addr 127.0.0.1:2250
```
