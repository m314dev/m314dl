# m314dl

A fast HLS (`.m3u8`) and DASH (`.mpd`) downloader in one static Go binary. It
decrypts CENC and cbcs streams (ISO/IEC 23001-7) with keys you supply, in the
same pass as the download, so there is no separate `mp4decrypt` or
`shaka-packager` step.

```sh
m314dl -o out.mp4 https://example.com/master.m3u8            # download an HLS stream
m314dl -key KID:KEY -o out.mp4 https://example.com/video.mpd # download + decrypt a DASH stream
```

If you already use [N_m3u8DL-RE](https://github.com/nilaoda/N_m3u8DL-RE),
[vsd](https://github.com/clitic/vsd), `yt-dlp` or `ffmpeg` for this, m314dl is a
single-binary alternative. It was faster than N_m3u8DL-RE and vsd in every case
of the benchmark below.

## Contents

- [Quick start](#quick-start)
- [Why I built it](#why-i-built-it)
- [Benchmarks](#benchmarks)
- [How decryption works](#how-decryption-works)
- [Responsible use](#responsible-use)
- [Features](#features)
- [Install](#install)
- [Usage](#usage)
- [Track selection](#track-selection)
- [Serving a live stream](#serving-a-live-stream)
- [Worker mode](#worker-mode)
- [RPC server](#rpc-server)
- [FAQ](#faq)
- [Tests](#tests)
- [License](#license)

## Quick start

```sh
# download an HLS or DASH stream (best video, best audio per language, all subs)
m314dl -o out.mp4 https://example.com/master.m3u8
m314dl -o out.mp4 https://example.com/manifest.mpd

# decrypt a CENC / cbcs DASH stream with a key you have (no mp4decrypt)
m314dl -key KID:KEY -o out.mp4 https://example.com/manifest.mpd

# list the streams in a manifest
m314dl -list https://example.com/manifest.mpd

# record a live stream for an hour (Ctrl-C any time and the file is still muxed)
m314dl -live-duration 1h -o show.mp4 https://example.com/live.m3u8

# serve what you download as a live HLS, TS or DASH endpoint
m314dl -serve :8314 https://example.com/master.m3u8
```

Run `m314dl -h` for the full flag list.

## Why I built it

For about a year my go-to tool was
[N_m3u8DL-RE](https://github.com/nilaoda/N_m3u8DL-RE). It's excellent, and it's
what got me far enough into this space to write my own downloader, so credit
where it's due.

I started m314dl because none of the tools I tried did everything I needed at
once: download and decrypt in one pass, run as a server, and re-serve a live
stream, all from one static binary. If your needs are simpler, N_m3u8DL-RE is
still a great pick.

## Benchmarks

Wall-clock time to download, decrypt and mux a 5-minute 1080p stream (about
150 MB, 75 segments per track), served from localhost with a simulated 50 ms CDN
round trip. Best of 3, and every output was checked to be full length (155 MB).
Lower is better.

| Case | **m314dl** | vsd 0.5.0 | N_m3u8DL-RE 0.6.0 |
|---|---|---|---|
| HLS TS (VOD) | **0.95 s** | 1.60 s | 1.12 s |
| HLS fMP4 (VOD) | **0.96 s** | 1.62 s | 1.08 s |
| DASH clear (VOD) | **0.91 s** | 1.85 s | 1.45 s |
| DASH CENC (AES-CTR) | **0.98 s** | 1.95 s | 2.69 s |
| DASH cbcs (AES-CBC pattern) | **0.99 s** | 1.88 s | 1.82 s |

Encryption adds about 0.07 s to m314dl's time. Each segment is decrypted in
memory as it arrives, overlapping the next fetch, and never touches disk. The
other two pay a second or more for the same content: N_m3u8DL-RE runs
`mp4decrypt` over the merged file afterwards (see
[How decryption works](#how-decryption-works)), and vsd downloads the streams
one at a time.

With no `-t` flag, m314dl tunes the number of requests in flight to the network
(AIMD, 4 to 64) and backs off from rate-limited servers, honoring `Retry-After`.
That hides the round-trip time without a thread count to guess. `-t N` uses
exactly N and holds it, like `--thread-count`, backing off only under real
rate limiting and then climbing straight back.

The corpus generator, the throttling server and the exact commands are in
[`bench/`](bench/). Run them on your own hardware rather than trusting a table
in someone's README.

Decryption alone runs at about 1.7 GB/s (CTR) and 2.0 GB/s (CBC) on one thread
(`go test -bench . ./internal/mp4/`), far above typical network bandwidth, and
every worker thread decrypts in parallel, so the network is the only bottleneck.
The parse and manifest-render benchmarks sit next to it:

```sh
go test ./internal/dash/ ./internal/hls/ ./internal/restream/ -run=^$ -bench=. -benchmem
M314_FIXTURES="$PWD/bench/fixtures" go test ./internal/mp4/ ./internal/engine/ -run=^$ -bench=.
```

## How decryption works

Most tools decrypt after the fact: they download the whole encrypted file, then
run `mp4decrypt` or `shaka-packager` over it. m314dl decrypts each fragment in
the download worker, in RAM, as soon as it arrives, in the same place it handles
AES-128. There is no encrypted temp file, no second pass over the data and no
subprocess per segment. Decryption runs on every thread at once and overlaps
with network I/O, which is why the encrypted rows in the benchmark are flat.

The schemes are `cenc` (AES-CTR), `cbcs` (AES-CBC with the crypt/skip pattern
and a constant IV), `cens` and `cbc1`, implemented from the ISO/IEC 23001-7
boxes (`tenc`, `senc`, `saiz`/`saio`, `trun`). The output is checked byte for
byte against `mp4decrypt`, and by hermetic round-trip tests.

You supply the content key. m314dl has no Widevine or PlayReady CDM and does no
license handshake; the default download paths of vsd and N_m3u8DL-RE don't
either.

```sh
# KID:KEY in hex (dashes in the KID are optional); repeat -key for multi-key manifests
m314dl -key 00112233445566778899aabbccddeeff:0123456789abcdef0123456789abcdef \
       -o out.mp4 https://example.com/manifest.mpd
```

TLS verification is on by default. Only an explicit `-insecure` turns it off.

## Responsible use

m314dl decrypts only with keys you already have. Use it on streams you own or
have the right to download, and check the terms of the service and the law where
you live. Issues asking how to get keys for a particular service will be closed.

## Features

### Formats and protocols

- HLS, TS and fMP4: master and media playlists, rendition groups (`AUDIO=` and
  `SUBTITLES=` are honored, so the best audio is the one that belongs to the
  chosen variant), byte ranges, `EXT-X-MAP` changes mid-stream, AES-128 decrypted
  in process (`data:` key URIs work), and audio-only variant detection.
- DASH: SegmentTemplate (`$Number$` and `$Time$`, with `%0Nd`), SegmentTimeline
  (including negative `@r`), SegmentList, SegmentBase (split into subsegments by
  their `sidx`, so a whole on-demand file never sits in RAM), multi-period
  manifests merged with discontinuity markers, namespace-agnostic lenient XML,
  and `cenc:default_KID`.
- Smooth Streaming (MSS): `.ism/Manifest` for VOD and live, `{bitrate}` and
  `{start time}` URL templates, `<c t d r>` timelines, H.264 and AAC. The
  presentation ships no init segments, so m314dl builds them from
  `CodecPrivateData`. PIFF fragments are normalized for players, and the
  PlayReady KID is read from the `ProtectionHeader` so `-key` can decrypt it.
- Plain files: point it at a direct file URL (`.mp4`, `.mkv`, `.zip`, anything)
  and it downloads over many byte-range connections at once, like aria2c, with
  the same resume and retry. It falls back to one connection when the server
  doesn't do ranges.
- M3U playlists, from a URL or a local file: each `#EXTINF` entry downloads as
  its own title to `<-o dir>/<group-title>/<title>.<ext>`, one after another.
  Choose entries with `-list`, `-pick 3,7-12`, `-group REGEX` or `-match REGEX`.
  Live channels are skipped, and so are titles that already finished when you
  rerun. `-titles N` downloads N titles at once under one combined progress
  line.

### Decryption

`cenc`, `cbcs`, `cens` and `cbc1` are decrypted natively, per fragment and in
memory, with output identical to the reference tools. HLS SAMPLE-AES (AAC and
H.264) works too when you supply the key.

### Download engine

- VOD and live share one pipeline: a feeder, a worker pool and an ordered
  writer. Segments stream to disk (one part file per in-flight segment, decrypted
  and appended in order), so peak memory is about one segment rather than one
  per thread, even when segments are huge.
- Without `-t`, requests in flight tune themselves to the network (AIMD, 4 to
  64) and back off on 429 and 5xx, honoring `Retry-After`. `-t N` pins exactly N
  and still backs off on rate limits.
- Every request in flight gets its own TCP connection over HTTP/1.1. If a CDN
  offers HTTP/2, Go multiplexes all concurrent segments onto one connection,
  and a single flow can't fill a real network's pipe because RTT and packet loss
  cap it. N_m3u8DL-RE, aria2 and yt-dlp open many connections for the same
  reason.
- Resume works after a hard kill as well as a clean exit. Segments stream to
  part files with HTTP byte-range resume and the checkpoint is written on every
  commit, so an interrupted VOD download continues byte-exact and loses at most
  the segment it was writing. A failed segment is never silently skipped.
- Retries use exponential backoff with jitter and also cover failures halfway
  through a response body. They look at the status: 404 fails fast, 5xx and 429
  retry.

### Live

- Recording works for HLS and DASH. It starts at the live edge
  (`-live-from-start` takes the whole DVR window), retries failed refreshes
  forever, and dedupes segments across refreshes with a bounded set, so a 24/7
  channel doesn't grow without limit. `-live-duration` sets a limit, and Ctrl-C
  stops the recording cleanly and muxes what you have.
- `-ad-keyword` skips ad segments by regex, on live refreshes too.

### Subtitles

WebVTT (deduped across concatenated segments), TTML to SRT (a lenient regex
parser that copes with non-compliant XML), and native fMP4 `wvtt` and `stpp`
extraction. `wvtt` timing comes from the MP4 sample metadata, including empty
gaps and cues split across samples. SRT and VTT conversion doesn't need FFmpeg.
Subtitles are muxed with ISO 639-2 language tags, or written as separate files
with `-sub-external`. If a conversion fails, the raw subtitle file is kept so you
can recover it, even after the video and audio muxed fine and without
`-keep-temp`.

### Input and scraping

- Input can be an HTTP(S) URL, as the first or the last argument; a local
  `.m3u8` or `.mpd` file or `file://` path, for manifests that are signed per
  request and never published; or a web page to scrape.
- Given a web page, it looks for `.m3u8` and `.mpd` URLs, including ones in
  inline JSON and one level of iframe.
- `-curl` takes a browser's "Copy as cURL" and pulls out the URL and all the
  headers, so you don't type each `-H` by hand. Pass it as a file or on stdin,
  because the shell mangles the quotes in a browser's curl command when it's
  inline:
  ```sh
  m314dl -curl - -o out.mp4 <<'CURL'
  curl 'https://host/master.m3u8' -H 'referer: https://site/' -H 'user-agent: …'
  CURL
  ```

### Networking and operations

- Custom headers (sent verbatim), Netscape `cookies.txt`, and HTTP or SOCKS
  proxies with auth.
- Plays well with scripts: plain progress lines when stdout isn't a terminal (no
  ANSI codes in your logs), real exit codes, and quiet, machine-readable output.
- Once a day it checks GitHub for a newer release and prints a one-line notice
  if you're behind. The check never blocks a download. `-no-update-check` or
  `M314DL_NO_UPDATE_CHECK=1` turns it off.

## Install

Prebuilt binaries for Linux, macOS and Windows (amd64 and arm64, plus armv7 on
Linux) are on the [releases page](https://github.com/m314dev/m314dl/releases).
Or use a package manager:

```sh
# macOS and Linux (Homebrew)
brew install m314dev/tap/m314dl

# Windows (Scoop)
scoop bucket add m314dev https://github.com/m314dev/scoop-bucket
scoop install m314dl

# with a Go toolchain
go install github.com/m314dev/m314dl@latest
```

m314dl needs `ffmpeg` to mux: on your PATH, next to the binary, or given with
`-ffmpeg <path>`. The Docker image already has it:

```sh
docker run --rm -v "$PWD:/data" ghcr.io/m314dev/m314dl -o out.mp4 https://example.com/master.m3u8
```

Files land in the current directory, which is what the `-v "$PWD:/data"` mount
does.

To build from source:

```sh
go build -o m314dl .
```

## Usage

```sh
# best video, best audio per language and all subtitles into out.mp4
m314dl -o out.mp4 https://example.com/master.m3u8

# decrypt a CENC / cbcs DASH stream with a key you have (no mp4decrypt)
m314dl -key KID:KEY -o out.mp4 https://example.com/manifest.mpd

# list streams
m314dl -list https://example.com/manifest.mpd

# pick streams: filters are regexes, colon-separated; for= picks from the matches
m314dl -sv 'res=1080' -sa 'lang=^en' -ss 'lang=en|de' -o out.mkv URL
m314dl -sv best -sa 'for=best2' URL

# record a live stream for 1 hour (or Ctrl-C any time; the file is still muxed)
m314dl -live-duration 1h -o show.mp4 https://example.com/live.m3u8

# scrape a page for the stream
m314dl -o video.mp4 https://example.com/watch/12345

# resume an interrupted VOD download: rerun the same command

# M3U playlist: number the entries, then download some of them into Movies/
m314dl -list playlist.m3u
m314dl -o Movies -group 'movies' -match 'batman' -t 1 playlist.m3u
# three titles at once, one connection each
m314dl -o Movies -pick 1-40 -titles 3 -t 1 playlist.m3u
```

Run `m314dl -h` for all flags.

## Track selection

`-sv`, `-sa` and `-ss` accept:

| Value | Meaning |
|---|---|
| `best`, `worst`, `all`, `best3` | position in the sorted list |
| `key=regex[:key=regex...]` | filter; keeps every match |
| `key!=regex` | keep streams that don't match (RE2 has no negative lookahead, so this is how you write a deny-list) |
| `...:for=bestN` | take the N best of the filtered set |
| `none` | drop this type |

Regex keys: `id`, `lang`, `name`, `codecs`, `res`, `range`, `channel`.
Numeric keys: `bwmin`/`bwmax` (kbps), `hmin`/`hmax`/`wmin`/`wmax` (pixels),
`segsmin`/`segsmax` (segment count), `plistdurmin`/`plistdurmax` (seconds, or
`20m`).

A numeric filter lets a stream through when the value is unknown. That way
`segsmin` and `plistdurmin` drop short ad and bumper periods in DASH without
touching HLS tracks whose length isn't known yet. If a height or width bound
excludes every rendition, m314dl falls back to the nearest one, so a quality
preference never takes a channel off the air. Video sorts by height, then
bandwidth; audio by the default flag, then channels, then bandwidth.

```sh
# drop 1-segment SSAI bumpers, keep 720p-or-better AVC content
m314dl -sv 'segsmin=2:plistdurmin=20m:res=x720$:codecs=avc1|avc3:for=best' URL
# drop two mislabeled subtitle tracks by id (a deny-list through negation)
m314dl -ss 'id!=^(a|b)$:for=all' URL
```

## Serving a live stream

Instead of writing a file, m314dl can publish what it downloads as a live stream
over HTTP. You can pull one feed and serve it to many viewers, or make a live
recording watchable while it records. Given a key, the served copy is decrypted.

```bash
# pull a stream and serve it as live HLS
m314dl -serve :8314 https://example.com/master.m3u8
# → http://localhost:8314/live.m3u8

# decrypt with your key and serve the clear copy
m314dl -serve :8314 -key KID:KEY https://example.com/manifest.mpd

# serve one continuous MPEG-TS instead (VLC, set-top boxes)
m314dl -serve :8314 -serve-format ts https://example.com/live.m3u8
# → http://localhost:8314/live.ts

# serve live DASH (dash.js, Shaka, ExoPlayer)
m314dl -serve :8314 -serve-format dash https://example.com/manifest.mpd
# → http://localhost:8314/live.mpd
```

Open `http://<host>:8314/live.m3u8` (HLS), `/live.ts` (MPEG-TS) or `/live.mpd`
(DASH) in VLC, mpv, hls.js, dash.js or any other player.

An FFmpeg-based restreamer works differently. Here the segments come out of the
normal download engine already decrypted and in playback order, and the
packager sits after selection, download and decryption. The source is fetched
once, no subprocess runs on the copy path, and the output segments are
byte-identical to the source. A single copy is kept in a shared window and
served to every viewer with Range and ETag support, rather than buffered per
connection.

### HLS output

- `GET /live.m3u8`: the multivariant (master) playlist
- `GET /{track}/index.m3u8`: a track's media playlist (`video`, `audio-en`, …)
- `GET /{track}/init.mp4`: the fMP4 init segment (the `EXT-X-MAP` target)
- `GET /{track}/000123.ts`: a media segment

A live source rolls a bounded in-memory window, with the right `EXT-X-VERSION`
for fMP4 or TS, `EXT-X-MAP` for fMP4, and measured `BANDWIDTH`, `CODECS` and
`RESOLUTION` in the master playlist. A finite (VOD) source is served as a
growing VOD you can seek in: segments spool to disk so RAM stays flat, the whole
timeline is scrubbable, and `EXT-X-ENDLIST` appears when it completes.

### MPEG-TS output

`GET /live.ts` is one endless transport stream sent to every viewer. Sending
never blocks: a viewer that falls a full buffer behind is dropped, and no other
viewer waits for it. (A naive broadcaster stalls everyone on the slowest
client.)

- A TS source needs no FFmpeg. The segments are already MPEG-TS, so they
  concatenate into a valid continuous stream without a remux. Each viewer joins
  on a segment boundary, which gives a clean, decodable start (PAT/PMT and a
  keyframe), and continuity counters are renumbered into one unbroken sequence
  per PID across boundaries.
- An fMP4 source, or separate video and audio, goes through one FFmpeg remux.
  m314dl runs a single long-lived FFmpeg that copies the decrypted tracks into
  TS and muxes in separate audio. It's one process for the whole stream, not one
  per segment, and there's no `-re` pacing because segment arrival paces it.
  Since m314dl decrypts upstream, FFmpeg only ever sees a clear copy, so the
  memory leak that FFmpeg restreamers hit with encrypted input doesn't apply.
  Pass `-serve-transcode '<ffmpeg args>'` to re-encode instead of copying, for
  example `-c:v libx264 -preset veryfast -c:a aac`.

### DASH output

`GET /live.mpd` serves a SegmentTemplate + SegmentTimeline manifest and the same
fMP4 segments, with one AdaptationSet per track. Live segments stay in memory
and VOD segments go to disk. A live source produces a `dynamic` MPD with
`minimumUpdatePeriod`.

VOD is presented like a live broadcast for now. The MPD stays `dynamic` until
the download has finished and the full published duration has passed since the
publisher started; only then does it turn `static` and fully seekable. This is
on purpose, for player compatibility: downloading a two-hour film in five
minutes doesn't make its MPD static after five minutes. DASH output needs an
fMP4 source; a TS source has to be remuxed first, which isn't built yet.

The first MPD request waits briefly for every track's first segment. If a track
is still missing, the server returns HTTP 503 with `Retry-After: 1` rather than
an MPD with an empty timeline. Retry once the source is ready.

Audio codecs, EAC3 (`ec-3`) included, pass through rather than being converted
to AAC, so the player has to support the codec you selected. With `-v`, the
logs list the selected video and audio tracks, in `-serve` and in worker mode.

HLS and DASH output only copy (same container family, no FFmpeg). MPEG-TS
output copies a TS source in pure Go and remuxes an fMP4 or separate-audio
source through one FFmpeg, with optional transcoding. TS to fMP4 for DASH and
HLS-fMP4 output isn't wired up yet, and neither is serving subtitles.

## Worker mode

`-serve` runs one presentation. `-worker` runs a multi-channel agent that a
controller drives over HTTP, starting and stopping channels on the fly, each
served at `/{id}/…`. One process holds all of them. The media is already in RAM
in the packager, so there's no subprocess to babysit and no reverse proxy to a
child's port.

```bash
# on the streaming server (a secret is required on non-loopback binds)
m314dl -worker :7001 -worker-secret mytoken -worker-max-channels 32

# start a channel (source to live HLS at /news/live.m3u8)
curl -H 'Authorization: Bearer mytoken' -X POST http://server:7001/api/channels \
  -d '{"id":"news","url":"https://…/manifest.mpd","format":"hls","keys":["KID:KEY"],
       "headers":{"Referer":"https://…"}}'

# ts / dash channels the same way (format: "ts" | "dash")
# list / inspect / stop
curl -H 'Authorization: Bearer mytoken' http://server:7001/api/channels
curl -H 'Authorization: Bearer mytoken' http://server:7001/api/channels/news
curl -H 'Authorization: Bearer mytoken' -X DELETE http://server:7001/api/channels/news
curl -H 'Authorization: Bearer mytoken' http://server:7001/api/health

# viewers play the media URL (unauthenticated, like any origin)
mpv http://server:7001/news/live.m3u8
```

Each channel uses the whole engine: selection, native decryption, live refresh
and the HLS, TS and DASH packagers above. A channel is exactly a `-serve` stream
under an id. The control plane has a bearer secret, an admission cap
(`-worker-max-channels`, which returns 503 past the limit) and a graceful
shutdown that stops every channel. State lives in memory on purpose: the worker
is stateless, and the controller's own reconciliation restarts channels after a
restart.

## RPC server

Run m314dl on a server and submit ordinary download jobs over HTTP/JSON. Each
job is a child m314dl process, so every flag, resume and graceful stop behaves
exactly as it does locally.

```bash
# on the server (a secret is required on non-loopback binds)
m314dl -rpc 0.0.0.0:8314 -rpc-secret mytoken -rpc-max-jobs 64

# submit a job (args are normal CLI flags)
curl -H 'Authorization: Bearer mytoken' -X POST http://server:8314/add \
  -d '{"url":"https://example.com/master.m3u8","args":["-o","movie.mp4","-key","KID:KEY"]}'
# → {"id":1}   (or HTTP 503 if -rpc-max-jobs are already running)

# watch: state, latest progress line, error if any
curl -H 'Authorization: Bearer mytoken' http://server:8314/jobs
curl -H 'Authorization: Bearer mytoken' http://server:8314/jobs/1   # + full log
curl -H 'Authorization: Bearer mytoken' http://server:8314/health   # {"running":3,"total":57,"max":64}

# stop gracefully (live: mux what's recorded; VOD: save resume state);
# call it twice to abort, same as pressing Ctrl-C twice
curl -H 'Authorization: Bearer mytoken' -X POST http://server:8314/jobs/1/stop
```

Output files land in the server's working directory unless `-o` gives a path.
An authenticated client has the full power of the CLI on that server, so treat
the secret like an SSH key.

It's meant to run unattended for weeks:

- Finished jobs are reaped after `-rpc-retain` (default 1h, with a hard
  ceiling) and each job's captured log is capped, so memory stays bounded.
- `-rpc-max-jobs` (default 64, `0` for unlimited) caps concurrent jobs. Past
  it, `/add` returns 503 instead of fork-bombing the host.
- Each job's output is buffered under its own lock, so hundreds of chatty jobs
  don't queue up behind a shared one.
- Job ids only go up. A reaped id is never reused, so a stale client can't reach
  a recycled job.
- On SIGINT or SIGTERM the server stops accepting, tells every running job to
  finish, then exits. On Linux, jobs die with the server even after a hard crash
  (`Pdeathsig`), so a crash never leaves orphaned downloads.
- Request bodies are size-limited, reading headers has a timeout, and the secret
  is compared in constant time.

## FAQ

### How do I download an HLS (`.m3u8`) stream?

`m314dl -o out.mp4 https://host/master.m3u8`. It fetches the master playlist,
picks the best video, the best audio and all subtitles by default, downloads the
segments in parallel and muxes them to MP4 with ffmpeg.

### How do I download a DASH (`.mpd`) stream?

Same command with the MPD URL: `m314dl -o out.mp4 https://host/manifest.mpd`.
SegmentTemplate, SegmentTimeline, SegmentList, SegmentBase and multi-period
manifests all work.

### How do I decrypt a CENC-encrypted stream without mp4decrypt?

Pass the content key: `m314dl -key KID:KEY -o out.mp4 https://host/manifest.mpd`.
Decryption (`cenc` AES-CTR and the `cbcs` AES-CBC pattern) runs in process, per
fragment, while the stream downloads. There's no `mp4decrypt` or
`shaka-packager` step. m314dl doesn't do the license or CDM handshake, so the key
has to come from you.

### How do I record a live stream?

`m314dl -live-duration 1h -o show.mp4 https://host/live.m3u8`. Recording starts
at the live edge. Press Ctrl-C at any point and the file is still finalized and
muxed.

### Is it faster than N_m3u8DL-RE or vsd?

On the benchmark above, yes, for HLS, DASH and encrypted content. The code to
run it yourself is in [`bench/`](bench/).

## Tests

```sh
go test ./...                      # unit + integration
go test -bench . ./internal/mp4/   # decryption throughput
```

The CENC tests check decryption two independent ways. Parity tests compare the
output byte for byte with `mp4decrypt` on real shaka-packaged fixtures (they skip
when the fixtures or `mp4decrypt` aren't present). Hermetic round-trip tests
build fragments in code and recover the plaintext with no external tools.

## License

MIT. See [LICENSE](LICENSE).
