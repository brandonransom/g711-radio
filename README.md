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

`config.json` contains the HTTP port, an optional whisper block, usage logging options, and a hierarchical `regions` map. It is read once at startup by `loadConfig` in `main.go`. Every setting in this README, including all transcription settings for both local and remote mode, lives in this one file. The only other file read is the optional `config.secrets.json` (see `config.secrets.json.example`), which overlays `pfxPassword`, `pfxKeyPassword`, `certFile`, `keyFile`, and `iceServers`. Changes take effect on restart.

It is **not tracked in git** — each deployment keeps its own ports, certificate paths, and stream list. Copy the template to create one:

```sh
cp config.example.json config.json
```

Runtime output (`config.json`, `config.secrets.json`, `usage.csv`, `g711-radio.log`, `audio/`, `transcripts/`, and certificate files) is ignored for the same reason: the running server rewrites those files continuously, and tracking them makes every `git pull` on a live server fail with "local changes would be overwritten".

```json
{
  "httpPort": 80,
  "audioLogDir": "audio",
  "usageLogFile": "usage.csv",
  "whisper": {
    "modelPath": "C:\\path\\to\\ggml-medium.bin",
    "serverBinaryPath": "C:\\path\\to\\whisper-server.exe",
    "workers": 3,
    "gapMs": 4000,
    "maxClipMs": 600000,
    "autoTranscribeMinClipMs": 20000,
    "autoTranscribeMaxClipMs": 120000
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
- `pfxFile`: TLS certificate as a PFX/PKCS#12 bundle; its passwords go in `config.secrets.json`. Alternatively set `certFile` + `keyFile` (PEM).
- `audioLogDir`: Directory for the primary, user-facing audio archive — recorded clips are written here, served over HTTP at `/audio/` for in-browser playback, and referenced by the clip/transcript history. Optional; recording is disabled without it (unless whisper is otherwise configured, in which case clips are still transcribed from a temp file but not persisted). Audio and transcripts are kept **indefinitely** — nothing in this codebase deletes them.
- `audioBackupDir`: Optional. When set, every recorded clip is also written, byte-for-byte, to this second directory (mirroring the same region/group/stream folder structure) — a redundant copy for disaster recovery. It's never served over HTTP or shown in the UI, and a write failure here (e.g. a temporarily unreachable network mount) is logged but never blocks the primary recording. Any path that behaves like a normal filesystem works, including a mapped network drive.
- `usageLogFile`: CSV file to log visitor usage (connect, disconnect, audio download, transcription requests). Useful for spreadsheets and reporting (optional; if omitted, logs are not persisted)
- `whisper` block: Optional transcription configuration — see [Transcription settings](#transcription-settings). Omit it entirely to run without transcription.

## Multicast Audio Streaming

Streams support listening on **up to 4 UDP ports simultaneously**, with intelligent port priority and automatic failover. This enables redundant encoder setups, multi-site aggregation, and fallback scenarios.

### Configuration

Each stream can use:
- **`udpPort`** (deprecated): Single UDP port (backward compatible)
- **`udpPorts`**: Array of 1-4 UDP ports
- **`multicastAddr`** (deprecated): Single multicast address
- **`multicastAddrs`**: Array of multicast addresses (one per port, or empty string for unicast)

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

Transcription uses whisper.cpp's built-in HTTP server, [whisper-server](https://github.com/ggml-org/whisper.cpp/tree/master/examples/server). It keeps the model loaded in memory, so clips skip the model-load step that running `whisper-cli` once per clip used to pay. It runs in one of two modes:

- **Local mode:** g711-radio launches and supervises `workers` whisper-server instances on `127.0.0.1` itself.
- **Remote mode:** you run whisper-server instances on another host and list their URLs. See [Remote Transcription Server](#remote-transcription-server).

whisper.cpp must be installed separately on whichever host runs whisper-server. It is **not** managed by `go mod tidy`. Building whisper.cpp produces `whisper-server` alongside `whisper-cli`.

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
3. Note the full path of `build\bin\Release\whisper-server.exe`. In local mode, set it as `serverBinaryPath` in `config.json`, or add that directory to your PATH and leave the default (`whisper-server`).
4. Note the full path of `ggml-medium.bin`. In local mode, set it as `modelPath` in `config.json`. In remote mode, pass it to whisper-server with `-m` (see [Remote Transcription Server](#remote-transcription-server)).

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
4. Install the server binary to your PATH:
   ```bash
   sudo cp build/bin/whisper-server /usr/local/bin/
   ```
5. Note the full path of `ggml-medium.bin`. In local mode, set it as `modelPath` in `config.json`. In remote mode, pass it to whisper-server with `-m` (see [Remote Transcription Server](#remote-transcription-server)).

#### Optional: GPU acceleration on Linux (NVIDIA)

If your server has a CUDA-capable GPU, build with CUDA support for significantly faster transcription:

```bash
cmake -B build -DGGML_CUDA=ON
cmake --build build --config Release -j$(nproc)
```

Requires CUDA toolkit (`nvidia-cuda-toolkit`) to be installed.

### Transcription settings

All transcription settings live in the `whisper` block of `config.json` (decoded into `whisperConfig` in `whisper.go`). If `remoteServers` is non-empty the server runs in **remote mode** and logs which local-mode settings it is ignoring; otherwise setting `modelPath` selects **local mode**.

| Setting | Mode | Default | Meaning |
|---|---|---|---|
| `remoteServers` | remote | `[]` | Base URLs of whisper-server instances, e.g. `["http://gpu-box:8080", "http://gpu-box:8081"]`. `http://` is assumed if omitted; a path prefix (whisper-server's `--request-path`) is kept. One worker per entry. |
| `serverBinaryPath` | local | `whisper-server` (on PATH) | whisper.cpp `whisper-server` executable. |
| `modelPath` | local | — | GGML model each local instance loads. |
| `workers` | local | `2` | Number of local whisper-server instances (i.e. parallel transcriptions). Each holds its own copy of the model in RAM/VRAM. |
| `localBasePort` | local | `18910` | Loopback port of the first instance; the rest use the next consecutive ports. |
| `serverArgs` | local | `[]` | Extra whisper-server **launch** flags, e.g. `["-t", "4", "-fa"]`. `-m`, `--host`, `--port`, `--request-path`, and `--inference-path` are set by g711-radio and rejected here. |
| `inferenceParams` | both | `{}` | Extra **per-request** `/inference` form fields; values may be strings, numbers, or booleans. `file` and `response_format` are reserved. |
| `timeoutMs` | both | `60000` | Limit for a single `/inference` request. |
| `gapMs`, `maxClipMs`, `autoTranscribeMinClipMs`, `autoTranscribeMaxClipMs` | both | see [How transcription works](#how-transcription-works) | Recording and auto-transcription controls. |

By default, requests send `language=en`, `no_timestamps=true`, `beam_size=5`, and `best_of=5`. These match the previous `whisper-cli` behavior; whisper-server's own defaults are greedy decoding with `best_of=2`. Any `inferenceParams` entry overrides them.

**Where to tune what.** whisper.cpp has two kinds of settings:

- **Per-request decoding settings** go in `inferenceParams` and work in both modes: `beam_size`, `best_of`, `temperature`, `temperature_inc`, `prompt`, `language`, `no_speech_thold`, `entropy_thold`, `logprob_thold`, `suppress_nst`, `audio_ctx`, `max_context`, and the VAD options (`vad`, `vad_threshold`, etc.). For example, `{"beam_size": -1, "best_of": 2}` switches to faster greedy decoding, and a `prompt` such as `"Forest Service dispatch radio traffic."` can improve domain vocabulary.
- **Load-time settings** are whisper-server command-line flags: the model, `-t` threads, `-p` processors, `-fa` flash attention, `-ng` (disable GPU), and `--vad-model`. In local mode, put them in `serverArgs`. In remote mode, pass them on the command line where you launch each instance.

Old settings `binaryPath` (whisper-cli) and `remoteHost` (the removed `cmd/whisper-server`) are rejected at startup, with an error that names the replacement setting.

If local mode can't start (missing binary or model), the server logs a warning and keeps streaming and recording with transcription disabled. A local instance that exits is restarted with backoff, and its last output is logged. On Windows (job object) and Linux (`Pdeathsig`), instances are also killed if g711-radio itself crashes, so they can't hold their ports.
### How transcription works

- Recording is presence-based: a WAV clip starts the moment a stream's incoming audio packets begin, with no voice/energy detection — the upstream source devices already gate transmission with their own VOX/squelch
- Gaps between packets of up to `gapMs` are bridged into the same clip (packets are simply concatenated in the order received, with no padding inserted for the gap — UDP delivery timing isn't reconstructed); a longer gap, or hitting `maxClipMs`, finalizes the clip
- Which finished clips get transcribed automatically is bounded by clip length, so short key-up blips and stuck-carrier marathons don't consume the worker pool:
  - **`autoTranscribeMinClipMs`** — clips shorter than this are not transcribed automatically. Leaving it unset (or `0`) disables automatic transcription entirely
  - **`autoTranscribeMaxClipMs`** — clips longer than this are not transcribed automatically. `0` (the default) means no upper limit
  - Both bounds are inclusive, so a clip qualifies when `autoTranscribeMinClipMs <= duration <= autoTranscribeMaxClipMs`. Every clip is still recorded and listed in the Recordings & Transcripts panel regardless; ones outside the window can be transcribed on demand with the panel's transcribe button. If the maximum is set below the minimum, no clip can satisfy both and the server logs a warning at startup
- The clip is queued for a pool of workers. Each worker is bound to one whisper-server instance and POSTs the WAV to its `/inference` endpoint. whisper-server processes one request at a time, so parallelism equals the number of instances. If an instance is unreachable, its clip goes back to the front of the queue for another instance; a clip is retried at most twice.
- Transcripts are broadcast to connected browsers via **Server-Sent Events** at `/transcripts`
- The individual stream page displays a live scrollable transcript panel
- Recording playback buttons queue clips; clicking the active recording's stop button ends that clip and advances to the next queued recording, if any
- Every transcript is also appended as a row to `transcripts.csv` inside `audioLogDir` (the primary audio archive directory, see [Config](#config)) — one row per transcript, with the transcribed WAV filename and stream name. This file lives alongside the audio clips it accompanies, is never pruned, and is separate from the per-stream JSON logs under `transcripts/` used for the in-browser history
- The "Recordings & Transcripts" panel (on both the individual stream page and the multi-stream region/forest pages) lets users browse the full, unpruned history rather than a fixed lookback window. The server scans each stream's WAV directory under `audioLogDir`, derives timestamps and durations from the files, and merges any matching text from the per-stream transcript log; recordings therefore remain visible across server restarts even if the separate transcript-event logs are missing. A date-range filter (last 24 hours / 7 days / 8 days (default) / 30 days / all time / a custom from–to range) controls what `GET /transcripts/history?streamId=<id>&since=<RFC3339>&until=<RFC3339>` fetches from the server — `since`/`until` are both optional, and omitting one means "from the beginning of recorded history" / "up to now" respectively. A recording-length filter (minimum seconds) and, on multi-stream pages, a stream filter are applied client-side against the fetched results, with no extra round-trip. **Download all filtered audio** sends the complete matching WAV set (including matching rows beyond the render cap) to `POST /recordings/download` and downloads a ZIP that preserves the archive's region/forest/stream folder structure. Rows render 300 at a time — newest first — to keep large "all time" views responsive; a **Load older** button above the oldest visible row pages in the next 300, so the entire history stays reachable without narrowing any filter

### Model selection

| Model | Size | Notes |
|-------|------|-------|
| `tiny` | 75 MB | Fast, low accuracy — not recommended for radio |
| `base` | 142 MB | Reasonable for clean speech |
| `small` | 466 MB | Good balance |
| `medium` | 1.5 GB | **Recommended** for radio audio quality |
| `large-v3` | 3 GB | Best accuracy, slowest |

## Remote Transcription Server

To offload transcription from the WebRTC host, run whisper.cpp's `whisper-server` on a more powerful machine and point `remoteServers` at it. No code from this repo runs on the transcription host.

**whisper-server has no authentication.** Run it only on a trusted private network, and firewall its ports so only the WebRTC host can reach them.

### 1. Install whisper.cpp on the transcription host

Follow [Install whisper.cpp on Windows](#install-whispercpp-on-windows) or [Install whisper.cpp on Linux](#install-whispercpp-on-linux) above (plus the GPU build if available).

### 2. Run one whisper-server instance per parallel transcription

Each instance transcribes one clip at a time and holds its own copy of the model. Start as many as the host's GPU, CPU, and RAM can run at once, each on its own port. Load-time tuning flags (`-t`, `-fa`, `-ng`, `-p`, and others) go here:

```bash
whisper-server -m /opt/whisper.cpp/models/ggml-medium.bin --host 0.0.0.0 --port 8080 -t 4 -fa &
whisper-server -m /opt/whisper.cpp/models/ggml-medium.bin --host 0.0.0.0 --port 8081 -t 4 -fa &
curl http://localhost:8080/health   # {"status":"ok"} once the model has loaded
```

To keep instances running across reboots and crashes, run each one as a service: systemd on Linux, or NSSM or a scheduled task on Windows.

### 3. Point the WebRTC host at them

In the WebRTC host's `config.json` (no whisper.cpp install needed there):

```json
{
  "whisper": {
    "remoteServers": ["http://whisper-host.local:8080", "http://whisper-host.local:8081"],
    "inferenceParams": { "beam_size": 5, "prompt": "Forest Service dispatch radio traffic." },
    "timeoutMs": 600000,
    "gapMs": 4000,
    "maxClipMs": 600000,
    "autoTranscribeMinClipMs": 20000
  }
}
```

One worker is bound to each URL, so listing two instances transcribes two clips at once. Per-request tuning (`inferenceParams`) is changed here and takes effect when g711-radio restarts; the remote instances keep running.

At startup g711-radio checks each server's `/health` and logs either `whisper server <url> is ready` or a `WARNING`. The check never blocks startup. If a server becomes unreachable, its worker requeues the clip for the others, polls `/health` with backoff, and resumes once the server is back.

To test an instance by hand:

```bash
curl http://whisper-host:8080/inference -F file=@clip.wav -F response_format=json
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
