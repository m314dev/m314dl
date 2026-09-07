# m314dl

A fast HLS, DASH and Microsoft Smooth Streaming downloader written in Go.

m314dl downloads VOD and live streams, handles track selection, subtitles, retries and resume, and can decrypt supported encrypted streams while they are being downloaded when you provide the content key.

It can also run as a restreamer, remote download server, or multi-channel streaming worker.

The project started as a downloader, but over time I ended up adding most of the things I wanted around the actual download pipeline instead of relying on several different tools.

## Quick start

Download an HLS stream:

```bash
m314dl -o video.mp4 https://example.com/master.m3u8
```

DASH works the same way:

```bash
m314dl -o video.mp4 https://example.com/manifest.mpd
```

List the available streams before downloading:

```bash
m314dl -list https://example.com/manifest.mpd
```

For encrypted DASH, if you already have the content key:

```bash
m314dl -key KID:KEY -o video.mp4 https://example.com/manifest.mpd
```

m314dl does not obtain DRM licenses or content keys. It only handles the media decryption when a key is supplied.

## What it supports

The main formats are:

* HLS (`m3u8`), both MPEG-TS and fragmented MP4
* MPEG-DASH (`mpd`)
* Microsoft Smooth Streaming (`.ism/Manifest`)
* VOD and live streams
* ordinary HTTP files

For HLS, this includes master/media playlists, alternate audio and subtitle renditions, byte ranges, changing `EXT-X-MAP` init segments, AES-128 and supported SAMPLE-AES streams.

For DASH, m314dl handles `SegmentTemplate`, `$Number$`, `$Time$`, `SegmentTimeline`, `SegmentList`, `SegmentBase`, multi-period manifests and the usual fMP4 layouts.

Smooth Streaming support includes VOD and live manifests, H.264/AAC tracks and PlayReady-encrypted fragments when the required key is provided.

## Download pipeline

One of the main things I wanted from m314dl was for downloading and processing to happen as one pipeline.

Segments are fetched concurrently and written in playback order:

```text
manifest
    |
    v
track selection
    |
    v
segment feeder
    |
    v
download workers
    |
    +--> retry / resume
    +--> decrypt when needed
    |
    v
ordered writer
    |
    +--> mux to file
    |
    +--> restream
```

The same basic engine is used for VOD and live streams.

Segments do not all have to sit in memory waiting for the download to finish. Workers use part files and the ordered writer commits completed segments in the correct sequence.

## Concurrent downloading

If you don't specify a thread count, m314dl adjusts concurrency automatically.

It starts conservatively, increases the number of requests while things are going well, and backs off when the server starts returning rate-limit or server errors.

`Retry-After` is honored when supplied by the server.

If you prefer a fixed number of workers:

```bash
m314dl -t 16 -o video.mp4 URL
```

The HTTP download path intentionally uses independent HTTP/1.1 connections for concurrent requests rather than putting all segment traffic through a single multiplexed connection.

## Resume

VOD downloads are checkpointed while they are running.

If a download is interrupted, run the same command again:

```bash
m314dl -o video.mp4 URL
```

Completed segments are not downloaded again.

A segment that was only partially downloaded can also continue using an HTTP byte range when the server supports it.

The checkpoint is updated as segments are committed, so resume does not depend on the process shutting down cleanly.

Failed segments are retried with exponential backoff and jitter. Mid-transfer read failures are retried as well, not only connection failures.

A segment that ultimately fails is treated as an error instead of being silently skipped.

## Native decryption

For supported formats, encrypted fragments are decrypted directly inside the download workers.

Currently implemented schemes include:

* CENC / `cenc` (AES-CTR)
* `cbcs`
* `cens`
* `cbc1`
* HLS AES-128
* supported HLS SAMPLE-AES AAC/H.264 streams

For example:

```bash
m314dl \
  -key 00112233445566778899aabbccddeeff:0123456789abcdef0123456789abcdef \
  -o video.mp4 \
  https://example.com/manifest.mpd
```

`-key` is repeatable for streams using multiple keys.

For fragmented MP4, the decryption code parses the encryption information from the MP4 boxes and decrypts samples while fragments pass through the normal download pipeline.

That means there is no need to download an encrypted file first and then run the entire result through `mp4decrypt` or `shaka-packager`.

m314dl is not a Widevine or PlayReady CDM and does not perform the license exchange. Key acquisition is outside the scope of the project.

## Track selection

By default, m314dl chooses the best video, appropriate audio tracks and available subtitles.

Use `-list` to inspect a manifest:

```bash
m314dl -list URL
```

Video, audio and subtitle tracks can then be selected with `-sv`, `-sa` and `-ss`.

For example:

```bash
m314dl \
  -sv 'res=1080' \
  -sa 'lang=^en' \
  -ss 'lang=en|de' \
  -o video.mkv \
  URL
```

Simple selectors are also available:

```bash
-sv best
-sv worst
-sv best3
-sv all
-sv none
```

Filters can be combined:

```bash
m314dl \
  -sv 'res=x720$:codecs=avc1|avc3:for=best' \
  URL
```

Supported filter fields include identifiers, language, name, codec, resolution, dynamic range, channel information, bandwidth, segment count and playlist duration.

Negation is supported as well:

```bash
-ss 'id!=^(a|b)$:for=all'
```

This can be useful with manifests containing unwanted periods, bumpers or incorrectly labelled tracks.

## Subtitles

m314dl can handle:

* WebVTT
* TTML
* `stpp` carried inside fragmented MP4

Segmented WebVTT is joined and duplicate cues are removed.

TTML can be converted to SRT, including some malformed/non-strict input that normal XML parsing may reject.

`stpp` samples are extracted directly from fragmented MP4.

Subtitles can be muxed into the final output or kept separately:

```bash
m314dl -sub-external -o video.mp4 URL
```

## Live recording

Live HLS and DASH can be recorded using the same downloader:

```bash
m314dl -live-duration 1h -o recording.mp4 https://example.com/live.m3u8
```

By default recording begins near the current live edge.

To start from the beginning of the currently available DVR window:

```bash
m314dl -live-from-start -o recording.mp4 URL
```

Live playlists are refreshed continuously and already-seen segments are deduplicated.

If refreshing the manifest temporarily fails, m314dl keeps retrying instead of immediately killing the recording.

Pressing Ctrl-C stops the recording gracefully and finalizes what has already been downloaded.

## Headers, cookies and proxies

Custom HTTP headers can be supplied directly:

```bash
m314dl \
  -H 'Referer: https://example.com/' \
  -H 'User-Agent: Mozilla/5.0' \
  -o video.mp4 \
  URL
```

Netscape-format `cookies.txt` files are supported, as are HTTP and SOCKS proxies with authentication.

For requests that depend on several browser headers, m314dl can also read a copied cURL command:

```bash
m314dl -curl request.txt -o video.mp4
```

or from stdin:

```bash
m314dl -curl - -o video.mp4 <<'CURL'
curl 'https://example.com/master.m3u8' \
  -H 'referer: https://example.com/' \
  -H 'user-agent: Mozilla/5.0'
CURL
```

The URL and headers are taken from the cURL command.

## Input doesn't have to be a manifest

You can give m314dl:

* an HTTP/HTTPS manifest URL
* a local `.m3u8` or `.mpd`
* a `file://` manifest
* a web page containing a stream
* a direct file URL

When given a normal web page, m314dl looks for HLS/DASH URLs referenced by the page, including common inline JSON cases and one level of iframe discovery.

For a normal file:

```bash
m314dl -o archive.zip https://example.com/archive.zip
```

m314dl uses parallel byte-range downloading when the server supports ranges and falls back to a normal single connection when it doesn't.

The same retry and resume behavior is used.

## Ad segment filtering

For live streams where unwanted segments can be identified from their URLs, `-ad-keyword` can be used to skip matching segments.

The filter is also applied as live manifests refresh.

## Restreaming

m314dl can send the output of its normal download pipeline to an HTTP server instead of writing only to a final file.

Start a restream:

```bash
m314dl -serve :8314 https://example.com/master.m3u8
```

The HLS output is then available at:

```text
http://localhost:8314/live.m3u8
```

Three output modes are currently available:

```bash
# HLS
m314dl -serve :8314 URL

# continuous MPEG-TS
m314dl -serve :8314 -serve-format ts URL

# DASH
m314dl -serve :8314 -serve-format dash URL
```

The restreamer sits after the normal selection/download/decryption path, so the source only has to be fetched once.

### HLS output

HLS mode keeps a rolling segment window in memory for live sources and generates the master and media playlists itself.

The same stored segment can be served to multiple viewers instead of creating a separate copy of the upstream stream for every client.

For finite sources, segments are spooled to disk and the output behaves as a growing seekable VOD until it completes.

### MPEG-TS output

When the source is already muxed MPEG-TS, m314dl can produce a continuous TS stream without FFmpeg.

Transport-stream continuity counters are rewritten across segment boundaries so the segments form one continuous stream.

For fMP4 or sources with separate audio/video tracks, one long-running FFmpeg process is used to copy-remux the tracks into TS.

Optional transcoding can be requested with:

```bash
-serve-transcode '<ffmpeg args>'
```

### DASH output

DASH serving generates an MPD and serves the downloaded fMP4 media directly.

The output timeline is built from the actual segment durations. Live input produces a dynamic MPD; finite input produces a static one.

## Worker mode

`-serve` handles one presentation.

For applications that need many independently controlled streams, m314dl can run as a worker:

```bash
m314dl \
  -worker :7001 \
  -worker-secret mytoken \
  -worker-max-channels 32
```

Channels are created through the HTTP API:

```bash
curl \
  -H 'Authorization: Bearer mytoken' \
  -X POST http://server:7001/api/channels \
  -d '{
    "id": "news",
    "url": "https://example.com/manifest.mpd",
    "format": "hls"
  }'
```

The resulting stream is available under the channel ID:

```text
http://server:7001/news/live.m3u8
```

A worker keeps all channels in one process. Each channel uses the same selection, downloading, decryption and packaging code as normal `-serve` mode.

The worker itself is intentionally stateless. A controller is expected to recreate the desired channels after a restart.

## RPC download server

There is a separate RPC mode for remotely starting normal m314dl download jobs:

```bash
m314dl \
  -rpc 0.0.0.0:8314 \
  -rpc-secret mytoken
```

Submit a job:

```bash
curl \
  -H 'Authorization: Bearer mytoken' \
  -X POST http://server:8314/add \
  -d '{
    "url": "https://example.com/master.m3u8",
    "args": ["-o", "video.mp4"]
  }'
```

Each job runs as a child m314dl process, so normal CLI options, resume and graceful shutdown behavior still apply.

Jobs can be listed and inspected through the API and stopped gracefully.

The server has limits for concurrent jobs, retained job history and captured logs so it can be left running without job state growing indefinitely.

Do not expose the RPC endpoint without authentication. An authenticated RPC client effectively has the same power as running m314dl from the command line on that machine.

## Installation

### Build from source

```bash
git clone https://github.com/mohaanymo/m314dl
cd m314dl
go build -o m314dl .
```

FFmpeg is required for final muxing and for the restream paths that require remuxing or transcoding.

m314dl looks for FFmpeg on `PATH` or next to the executable.

A specific binary can also be supplied:

```bash
m314dl -ffmpeg /path/to/ffmpeg -o video.mp4 URL
```

### Docker

The Docker image includes FFmpeg:

```bash
docker build -t m314dl .
```

Example:

```bash
docker run --rm \
  -v "$PWD:/data" \
  m314dl \
  -o video.mp4 \
  https://example.com/master.m3u8
```

## Benchmarks

### Microbenchmarks

These are `go test` microbenchmarks over the hot paths. They are reproducible on your own machine:

```bash
# decryption and manifest/playlist rendering (no external corpus needed)
go test ./internal/dash/ ./internal/hls/ ./internal/restream/ -run=^$ -bench=. -benchmem

# native decryption and the full download->decrypt->commit pipeline
# (uses the small corpus from bench/gen-fixtures.sh)
M314_FIXTURES="$PWD/bench/fixtures" \
  go test ./internal/mp4/ ./internal/engine/ -run=^$ -bench=. -benchmem
```

Measured on an Intel Core i5-6200U (2 cores / 4 threads, 2.3 GHz), Go 1.27, Linux. Your numbers will differ; treat these as relative, not absolute.

| Benchmark | What it measures | Result |
|---|---|---|
| `DecryptFragment/dash-cenc` | native CENC (AES-CTR) decrypt of a ~2 MB fMP4 fragment | **1.68 GB/s** |
| `DecryptFragment/dash-cbcs` | native cbcs (AES-CBC pattern) decrypt of the same fragment | **2.05 GB/s** |
| `PipelineCENC` | full pipeline: concurrent download + CENC decrypt + in-order commit of ~375 MB over localhost | **~10 GB/s** effective |
| `DASHManifest` (cached) | serving the MPD for a 3000-segment VOD | **7.5 µs**, 15 allocs |
| `DASHManifest` (uncached) | same, rebuilding the timeline every request | 49.5 µs, 33 allocs |
| `Parse` (DASH) | parsing a 4-period × 6-rendition × 100-segment manifest (2400 `<S>`) | 16.8 ms |
| `ParseMedia` (HLS) | parsing a 3000-segment media playlist | 4.9 ms |

Two things worth calling out:

* The `PipelineCENC` figure has no network latency (everything is localhost), so it measures the download → decrypt → commit ceiling, not a network-bound real-world speed. It exists to show that decrypt-while-downloading is not the bottleneck: on this old laptop the pipeline moves media far faster than any single connection delivers it.
* The cached vs uncached manifest rows show why serving a long growing VOD to many polling players stays cheap: the timeline body is rebuilt only when a segment is added, not on every request.

### End-to-end comparison

There is also a reproducible end-to-end setup in [`bench/`](bench/) that compares HLS, fMP4 HLS, clear DASH and encrypted DASH downloads against other downloaders under a simulated CDN round-trip. That one needs the competitor binaries and the generated corpus, so it is kept in the repository rather than as a fixed table here:

```bash
cd bench
```

The main performance-related design choice in m314dl is that encrypted fragments are decrypted by the workers while downloading. There isn't a second full-file decryption pass after the download completes.

## A note about DRM

m314dl contains media decryption code, not a DRM client.

It does not:

* perform a Widevine license exchange
* perform a PlayReady license exchange
* extract content keys
* bypass a CDM

When encrypted media is used, you provide the content key yourself:

```text
KID:KEY
```

m314dl then uses that key to decrypt supported media formats.

Only use m314dl with streams and keys you are authorized to access.

## Command line options

The README only covers the commonly used options.

Run:

```bash
m314dl -h
```

for the complete list of flags.

## Reporting problems

Streaming manifests in the wild have a lot of strange edge cases.

If you find a stream that m314dl parses incorrectly, a download that doesn't resume correctly, a subtitle format it mishandles, or a case where another downloader works and m314dl doesn't, open an issue.

When possible, include the manifest or a minimal reproducible example. For authenticated streams, a sanitized manifest that still reproduces the problem is much more useful than credentials or private URLs.

## License

MIT
