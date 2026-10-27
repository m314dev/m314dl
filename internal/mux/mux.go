// Package mux runs ffmpeg to merge the downloaded raw streams into the final
// output file.
package mux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/m314dev/m314dl/internal/manifest"
)

type Input struct {
	Path     string
	Type     manifest.MediaType
	Language string
	Name     string
	Default  bool
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// FindFFmpeg finds the ffmpeg binary. An explicit value (from the -ffmpeg
// flag) always wins. It may be an absolute path, a relative path, or a bare
// name found on PATH. Without one, FindFFmpeg looks next to our own binary
// first, then on PATH.
func FindFFmpeg(explicit string) (string, error) {
	if explicit != "" {
		if p, err := exec.LookPath(explicit); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("ffmpeg not usable at %q", explicit)
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), exeName("ffmpeg"))
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	return exec.LookPath("ffmpeg")
}

// subCodec returns the subtitle codec to use for the output container, chosen
// by file extension. An empty result means no -c:s flag is passed.
func subCodec(ext string) string {
	switch ext {
	case ".mp4", ".m4v", ".mov":
		return "mov_text"
	case ".mkv":
		return "srt"
	case ".webm":
		return "webvtt"
	}
	return ""
}

// Mux merges inputs into outPath. The container type comes from the file
// extension. Streams are copied as they are, with no re-encoding. On failure
// the error carries the last part of ffmpeg's stderr output.
func Mux(ffmpeg string, inputs []Input, outPath string) error {
	if len(inputs) == 0 {
		return fmt.Errorf("mux: no inputs")
	}
	ext := strings.ToLower(filepath.Ext(outPath))
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	for _, in := range inputs {
		args = append(args, "-i", in.Path)
	}
	haveAudioInput := false
	for _, in := range inputs {
		if in.Type == manifest.Audio {
			haveAudioInput = true
		}
	}
	for i, in := range inputs {
		switch in.Type {
		case manifest.Audio:
			args = append(args, "-map", fmt.Sprintf("%d:a", i))
		case manifest.Video:
			if haveAudioInput {
				// A separate audio stream was chosen, so take only the video
				// from this input. Any audio muxed into the video variant
				// would otherwise show up as a duplicate track.
				args = append(args, "-map", fmt.Sprintf("%d:v", i))
			} else {
				args = append(args, "-map", fmt.Sprint(i))
			}
		default:
			args = append(args, "-map", fmt.Sprint(i))
		}
	}
	// -dn drops the data tracks (metadata, emsg) that CMAF init segments often
	// carry.
	args = append(args, "-dn", "-c:v", "copy", "-c:a", "copy")
	if sc := subCodec(ext); sc != "" {
		args = append(args, "-c:s", sc)
	}
	ai, si := 0, 0
	for _, in := range inputs {
		switch in.Type {
		case manifest.Audio:
			if in.Language != "" {
				args = append(args, fmt.Sprintf("-metadata:s:a:%d", ai), "language="+lang639(in.Language))
			}
			if in.Default {
				args = append(args, fmt.Sprintf("-disposition:a:%d", ai), "default")
			}
			ai++
		case manifest.Subtitles:
			if in.Language != "" {
				args = append(args, fmt.Sprintf("-metadata:s:s:%d", si), "language="+lang639(in.Language))
			}
			if in.Name != "" {
				args = append(args, fmt.Sprintf("-metadata:s:s:%d", si), "title="+in.Name)
			}
			// Mark the first subtitle track as default, so a player turns it on
			// without the viewer having to pick a track.
			if si == 0 {
				args = append(args, fmt.Sprintf("-disposition:s:%d", si), "default")
			}
			si++
		}
	}
	if ext == ".mp4" || ext == ".m4v" || ext == ".mov" {
		args = append(args, "-movflags", "+faststart")
	}
	if ext == ".ts" {
		args = append(args, "-f", "mpegts")
	}
	args = append(args, outPath)
	cmd := exec.Command(ffmpeg, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return fmt.Errorf("ffmpeg mux failed: %w\n%s", err, msg)
	}
	return nil
}

// ExtractSubtitle converts a raw subtitle stream file into outPath. The input
// may be fragmented MP4 (fMP4) carrying wvtt or stpp subtitles, or anything
// else ffmpeg understands. The output format (.srt or .vtt) follows the
// extension of outPath.
func ExtractSubtitle(ffmpeg, inPath, outPath string) error {
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", inPath, outPath)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg subtitle extract failed: %w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
