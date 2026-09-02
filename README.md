# G.711 Radio

A Go WebRTC server built with Pion. It reads a local JSON config file, listens on one UDP port per configured stream, extracts a 160-byte G.711 audio frame from each packet, and broadcasts each stream to browser clients over WebRTC. Optionally transcribes audio using whisper.cpp, either installed locally or offloaded to a separate remote host.

## Prerequisites

- **Go 1.24+**
- *(Optional)* **whisper.cpp** for server-side transcription — required on whichever host actually performs transcription (see [Transcription](#transcription) and [Remote Transcription Server](#remote-transcription-server) below)

## Run it

```bash
go mod tidy
go run .
```

Edit `config.json`, send your UDP audio to the configured ports, then open `http://localhost:<httpPort>`.

## Config

`config.json` contains the HTTP port, an optional whisper block, usage logging options, and a hierarchical `regions` map:

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

## Multicast Audio Streaming

Streams support listening on **up to 4 UDP ports simultaneously**, with intelligent port priority and automatic failover. This enables redundant encoder setups, multi-site aggregation, and fallback scenarios.

### Configuration

Each stream can use:
- **`udpPort`** (deprecated): Single UDP port (backward compatible)
- **`udpPorts`**: Array of 1-4 UDP ports
- **`multicastAddr`** (deprecated): Single multicast address
- **`multicastAddrs`**: Array of multicast addresses (one per port, or empty string for unicast)
- **`mping.commandPath`**: Directory used when opening the mping command window

Example:
```json
{
  "regions": {
    "Alaska": {
      "Chugach NF": [
        { "streamName": "Single Port", "udpPort": 5000 },
        { "streamName": "Redundant", "udpPorts": [5000, 5001] },
        { "streamName": "Multicast", "udpPorts": [5000, 5001], "multicastAddrs": ["224.0.0.1", ""] }
      ]
    }
  }
}
```

### Port Priority & Failover

- **Port Priority**: The first port to receive a valid audio packet becomes active; packets from other ports are silently dropped
- **Dropout Detection**: After ~1 second with no packets on the active port, the stream resets and any port can become active
- **Use Case**: Seamless failover from primary to backup encoder without manual intervention

### Backward Compatibility

Existing configs with single `udpPort` continue to work unchanged. Multicast is purely opt-in.

See the section below for detailed multicast configuration examples.

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

Transcription uses [whisper.cpp](https://github.com/ggerganov/whisper.cpp) running as a subprocess on whichever host actually performs transcription — the WebRTC server itself by default, or a separate host when using [Remote Transcription Server](#remote-transcription-server) below. It must be installed separately on that host — it is **not** managed by `go mod tidy`.

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
4. Note the full path of `ggml-medium.bin` — set it as `modelPath` in `config.json` (local mode) or `whisper-server.config.json` (remote mode; see [Remote Transcription Server](#remote-transcription-server) below).

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
5. Note the full path of `ggml-medium.bin` — set it as `modelPath` in `config.json` (local mode) or `whisper-server.config.json` (remote mode; see [Remote Transcription Server](#remote-transcription-server) below).

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

Transcription is CPU-heavy. Instead of running `whisper-cli` on the same machine as the WebRTC server, you can offload it to a separate, more powerful host using `cmd/whisper-server` — a small standalone HTTP server included in this repo. Two hosts are involved:

- **Transcription host** — runs `cmd/whisper-server` and needs `whisper.cpp` installed.
- **WebRTC host** — runs the main `g711-radio` app; in this mode it needs no whisper.cpp install at all, just network access to the transcription host's port.

**There is no authentication.** This is intended for a trusted internal/private network only — firewall the configured port (default `8090`) so it's reachable only from the WebRTC host.

### 1. Install whisper.cpp on the transcription host

On the machine that will run `cmd/whisper-server` (**not** the WebRTC host), follow the same install steps as local mode: [Install whisper.cpp on Windows](#install-whispercpp-on-windows) or [Install whisper.cpp on Linux](#install-whispercpp-on-linux) above. Note the resulting `whisper-cli` and model paths — you'll need them in step 2.

### 2. Build, configure, and run whisper-server

Get this repo onto the transcription host (clone it, or copy just what you need) and build the server:

```bash
go build -o whisper-server ./cmd/whisper-server
```

or run it directly without building a binary first:

```bash
go run ./cmd/whisper-server -config whisper-server.config.json
```

Copy `cmd/whisper-server/config.example.json` to `whisper-server.config.json` (or any path of your choosing; override with `-config`, which defaults to `whisper-server.config.json` in the current directory) and fill in the paths from step 1:

```json
{
  "port": 8090,
  "binaryPath": "/usr/local/bin/whisper-cli",
  "modelPath": "/opt/whisper.cpp/models/ggml-medium.bin",
  "workers": 4,
  "timeoutMs": 60000
}
```

- `port`: TCP port to listen on (default `8090`)
- `binaryPath`: Path to (or name on `PATH` of) the `whisper-cli` executable (default `whisper-cli`)
- `modelPath`: Path to the whisper.cpp GGML model file — **required**; the server refuses to start if this is empty
- `workers`: Maximum number of concurrent `whisper-cli` subprocesses (default `2`)
- `timeoutMs`: Per-request timeout for the `whisper-cli` subprocess, in milliseconds (default `60000`)

Start it:

```bash
./whisper-server -config whisper-server.config.json
```

Then confirm it's up:

```bash
curl http://localhost:8090/healthz
# {"status":"ok","workers":4,"inFlight":0,"totalProcessed":0}
```

### 3. Point the WebRTC host at it

The WebRTC host does **not** need whisper.cpp installed in this mode. Just set `remoteHost` in its `whisper` config block (see [Config](#config) above) to the transcription host's address, in `config.json`:

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

`binaryPath`/`modelPath` are omitted here — they belong in the transcription host's `whisper-server.config.json` from step 2 instead. The `workers` value here still controls how many clips this host will send to the remote server concurrently.

Start (or restart) the main app. On startup it performs a one-time, non-fatal reachability check against the remote host and logs one of:

```
remote whisper server at http://whisper-host.local:8090 is reachable
```

```
WARNING: remote whisper server at http://whisper-host.local:8090 unreachable: <error>
```

This check never blocks startup — the app starts either way — so treat the warning as a prompt to double-check connectivity/firewalling rather than a fatal error.

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

## Multicast Configuration Examples

### Example 1: Encoder Failover (Redundant Ports)

```json
{
  "regions": {
   "Alaska": {
     "Chugach NF": [
       {
         "streamName": "Fire Dispatch",
         "udpPorts": [5000, 5001]
       }
     ]
   }
  }
}
```

Primary encoder sends to port 5000; backup sends to port 5001. If primary fails (no packets for 1 second), backup takes over automatically.

### Example 2: Multicast with Unicast Fallback

```json
{
  "streamName": "Regional Broadcast",
  "udpPorts": [5000, 5001],
  "multicastAddrs": ["224.0.0.1", ""]
}
```

Primary encoder sends to multicast group 224.0.0.1:5000. Backup (outside the multicast network) sends to unicast port 5001. Primary wins if both are transmitting.

### Example 3: Multi-Site Aggregation

```json
{
  "streamName": "Forest Network",
  "udpPorts": [5000, 5001, 5002, 5003],
  "multicastAddrs": [
   "224.0.1.1",
   "224.0.1.2",
   "224.0.1.3",
   "224.0.1.4"
  ]
}
```

Four regional sites each broadcast to different multicast groups. Audio from all sites is aggregated; port priority ensures one encoder wins at any time.

### Testing Multicast

To send test audio to a multicast address:

```bash
# Using FFmpeg (Linux/macOS)
ffmpeg -f lavfi -i sine=frequency=1000:duration=30 \
  -acodec pcm_mulaw -ar 8000 -ac 1 \
  -f rtp "rtp://224.0.0.1:5000?ttl=32"
```

To verify packets arrive:

```bash
# Monitor multicast traffic
sudo tcpdump -i eth0 -n host 224.0.0.1 and port 5000
```

## Notes

- The server assumes 8 kHz mono G.711 PCMU frames with a 12-byte transport header and 160 audio bytes per packet (20 ms).
- The client loads streams from `/streams`, renders them as a hierarchical region → group → stream accordion, and opens each feed in its own dedicated tab.
- The client uses non-trickle ICE and is intended for local or LAN use. For internet-facing deployments, add STUN/TURN configuration.
- Transcription requires Chrome or Edge on the client (SSE is supported in all modern browsers; the panel displays regardless).
- Multicast streams support up to 4 ports per stream. Port priority ensures seamless failover in redundant encoder scenarios.

## Test with GStreamer

An example sender lives at `examples/gst-launch-pcmu-sine.sh`. It generates an 8 kHz mono sine wave, encodes it as PCMU, packetizes it as RTP, and sends it to `127.0.0.1:2250`.

## Debugging with pcapng

```bash
go run ./cmd/replay-pcap -pcap g711.pcapng -addr 127.0.0.1:2250
```
