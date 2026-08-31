# G.711 Radio

A Go WebRTC server built with Pion. It reads a local JSON config file, listens on one UDP port per configured stream, extracts a 160-byte G.711 audio frame from each packet, and broadcasts each stream to browser clients over WebRTC. Optionally transcribes audio using a locally-installed whisper.cpp instance.

## Prerequisites

- **Go 1.24+**
- *(Optional)* **whisper.cpp** for server-side transcription — required on whichever host actually performs transcription (see [Transcription](#transcription) and [Remote Transcription Server](#remote-transcription-server) below)

## Run it

```bash
go mod tidy
go run .
```

Edit the gitignored `config.local.json`, send your UDP audio to the configured ports, then open `http://localhost:<httpPort>`.

## Config

`config.local.json` contains the HTTP port, an optional whisper block, usage logging options, and a hierarchical `regions` map:

```json
{
  "httpPort": 8080,
  "audioLogDir": "audio",
  "usageLogFile": "usage.csv",
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

- `httpPort`: HTTPS port to listen on (default 443)
- `audioLogDir`: Directory to store recorded audio clips (optional)
- `usageLogFile`: CSV file to log visitor usage (connect, disconnect, audio download, transcription requests). Useful for spreadsheets and reporting (optional; if omitted, logs are not persisted)
- `whisper` block: Optional transcription configuration
  - `remoteHost`: Optional. If set (e.g. `"192.168.1.50:8090"` or `"whisper-host.local:8090"`), transcription requests are sent over HTTP to that host's `/transcribe` endpoint instead of running `whisper-cli` locally. May include an `http://`/`https://` scheme prefix; defaults to `http://` when omitted. When `remoteHost` is set, `binaryPath`/`modelPath` are unnecessary here — they belong in the remote server's own config instead. See [Remote Transcription Server](#remote-transcription-server).

The `whisper` block is optional. Omit it entirely to run without transcription.

## Usage Logging

When `usageLogFile` is configured, the server writes visitor activity to a CSV file with the following columns:
- **timestamp**: ISO 8601 timestamp of the event
- **action**: Event type (connect, disconnect, audio_download, transcript_request)
- **client_ip**: IP address of the visitor
- **stream**: Stream name
- **peer_id**: Unique peer ID for WebRTC connections
- **clip_id**: Clip ID for transcription requests
- **source**: Where the clip came from (registry or audiourl_fallback)
- **duration_ms**: Connection duration in milliseconds
- **path**: URL path for audio downloads

This CSV is spreadsheet-friendly and can be imported into Excel, Google Sheets, or used for analytics dashboards.

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

## Remote Transcription Server

Transcription is CPU-heavy. Instead of running `whisper-cli` on the same machine as the WebRTC server, you can offload it to a separate, more powerful host using `cmd/whisper-server` — a small standalone HTTP server included in this repo.

Set `whisper.remoteHost` in the main app's config to point at that host, and the main app will POST recorded WAV clips to it over HTTP instead of shelling out to `whisper-cli` locally. The remote host still needs `whisper.cpp` installed (see [Transcription](#transcription) above), but the WebRTC/G.711 host does not.

**There is no authentication.** This is intended for a trusted internal/private network only — firewall the configured port (default `8090`) if the remote host is otherwise reachable.

### Build and run

```bash
go build -o whisper-server ./cmd/whisper-server
./whisper-server -config whisper-server.config.json
```

or run it directly without building a binary:

```bash
go run ./cmd/whisper-server -config whisper-server.config.json
```

`-config` defaults to `whisper-server.config.json` in the current directory if omitted.

### Config file

Copy `cmd/whisper-server/config.example.json` to `whisper-server.config.json` (or any path you like) and edit it:

```json
{
  "port": 8090,
  "binaryPath": "whisper-cli",
  "modelPath": "/path/to/ggml-medium.bin",
  "workers": 2,
  "timeoutMs": 60000
}
```

- `port`: TCP port to listen on (default `8090`)
- `binaryPath`: Path to (or name on `PATH` of) the `whisper-cli` executable (default `whisper-cli`)
- `modelPath`: Path to the whisper.cpp GGML model file — **required**; the server refuses to start if this is empty
- `workers`: Maximum number of concurrent `whisper-cli` subprocesses (default `2`)
- `timeoutMs`: Per-request timeout for the `whisper-cli` subprocess, in milliseconds (default `60000`)

### Endpoints

- **`POST /transcribe`** — body is the raw WAV file bytes (`Content-Type: audio/wav`). An optional `X-Stream-Name` header is accepted purely for log context. Responds `200` with `{"text": "..."}` on success, or a `4xx`/`5xx` status with `{"error": "..."}` on failure.

  ```bash
  curl -X POST --data-binary @clip.wav -H "Content-Type: audio/wav" http://whisper-host:8090/transcribe
  ```

- **`GET /healthz`** — reports liveness and load:

  ```bash
  curl http://whisper-host:8090/healthz
  # {"status":"ok","workers":2,"inFlight":0,"totalProcessed":42}
  ```

### End-to-end example

On the dedicated transcription host, `whisper-server.config.json`:

```json
{
  "port": 8090,
  "binaryPath": "/usr/local/bin/whisper-cli",
  "modelPath": "/opt/whisper.cpp/models/ggml-medium.bin",
  "workers": 4,
  "timeoutMs": 60000
}
```

On the WebRTC server host, the `whisper` block in `config.local.json`:

```json
{
  "whisper": {
    "remoteHost": "whisper-host.local:8090",
    "workers": 3,
    "vadThreshold": 0.02,
    "silenceMs": 600,
    "minClipMs": 300,
    "maxClipMs": 30000
  }
}
```

`binaryPath`/`modelPath` are omitted on the client side since transcription now happens on `whisper-host.local`. The client-side `workers` value still controls how many clips can be sent to the remote server concurrently.

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
