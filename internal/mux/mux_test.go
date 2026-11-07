package mux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m314dev/m314dl/internal/manifest"
)

func TestFindFFmpegExplicit(t *testing.T) {
	// An explicit bare name that exists on PATH resolves. "sh" is always there.
	if p, err := FindFFmpeg("sh"); err != nil || p == "" {
		t.Fatalf("explicit name on PATH should resolve, got %q err=%v", p, err)
	}
	// A bad explicit path returns an error instead of falling back to the other
	// lookups.
	if _, err := FindFFmpeg("/no/such/ffmpeg-xyz"); err == nil {
		t.Fatal("bogus explicit ffmpeg path should error")
	}
}

func TestMuxArgsDolbyVision(t *testing.T) {
	video := Input{Path: "v.ts", Type: manifest.Video, Codecs: "dvh1.05.06"}
	audio := Input{Path: "a.ts", Type: manifest.Audio, Codecs: "mp4a.40.2"}
	args := strings.Join(muxArgs([]Input{video, audio}, "out.mp4"), " ")
	for _, want := range []string{"-tag:v dvh1", "-strict unofficial"} {
		if !strings.Contains(args, want) {
			t.Errorf("mp4 args miss %q: %s", want, args)
		}
	}
	// dvhe is the other sample entry name for the same profiles.
	if a := strings.Join(muxArgs([]Input{{Type: manifest.Video, Codecs: "dvhe.05.06"}}, "o.mp4"), " "); !strings.Contains(a, "-tag:v dvh1") {
		t.Errorf("dvhe should be tagged dvh1: %s", a)
	}
	// Profile 8 and plain HEVC keep ffmpeg's own tag.
	for _, c := range []string{"hvc1.2.4.L153.B0", "hev1.2.4.L150.B0", "avc1.640028", ""} {
		if a := strings.Join(muxArgs([]Input{{Type: manifest.Video, Codecs: c}}, "o.mp4"), " "); strings.Contains(a, "-tag:v") {
			t.Errorf("codecs %q should not force a tag: %s", c, a)
		}
	}
	// Matroska has its own Dolby Vision signalling; dvh1 there is wrong.
	if a := strings.Join(muxArgs([]Input{video}, "o.mkv"), " "); strings.Contains(a, "-tag:v") {
		t.Errorf("mkv should not force a tag: %s", a)
	}
}

// TestMuxKeepsDolbyVisionFromTS builds a small HEVC transport stream, marks it
// as Dolby Vision profile 5 in the PMT the way a broadcaster would, muxes it
// to MP4 through Mux, and checks that the MP4 carries the dvh1 sample entry
// and the Dolby Vision configuration record. Issue #13 was exactly this being
// lost. Skips when ffmpeg, ffprobe or libx265 are not around.
func TestMuxKeepsDolbyVisionFromTS(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip("ffmpeg not found")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "plain.ts")
	gen := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=25:duration=1",
		"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=none", "-f", "mpegts", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot make an HEVC TS here: %v %s", err, out)
	}
	ts, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dv := filepath.Join(dir, "dv.ts")
	if err := os.WriteFile(dv, addDolbyVisionDescriptor(t, ts), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mp4")
	if err := Mux(ffmpeg, []Input{{Path: dv, Type: manifest.Video, Codecs: "dvh1.05.06"}}, out); err != nil {
		t.Fatal(err)
	}
	probe, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_tag_string:stream_side_data_list", "-of", "flat", out).Output()
	if err != nil {
		t.Fatal(err)
	}
	p := string(probe)
	if !strings.Contains(p, `codec_tag_string="dvh1"`) {
		t.Errorf("sample entry is not dvh1:\n%s", p)
	}
	if !strings.Contains(p, "DOVI configuration record") || !strings.Contains(p, "dv_profile=5") {
		t.Errorf("no Dolby Vision profile 5 configuration in the MP4:\n%s", p)
	}
}

// addDolbyVisionDescriptor appends a DOVI_video_stream_descriptor (tag 0xB0,
// profile 5, level 6, RPU present, base layer present) to the HEVC entry of
// the PMT in a transport stream and fixes the section length and CRC.
func addDolbyVisionDescriptor(t *testing.T, ts []byte) []byte {
	t.Helper()
	const pkt = 188
	pmtPID := -1
	for off := 0; off+pkt <= len(ts); off += pkt {
		p := ts[off : off+pkt]
		pid := int(p[1]&0x1f)<<8 | int(p[2])
		if pid != 0 { // PAT
			continue
		}
		sec := p[5+int(p[4]):]                       // skip pointer field
		pmtPID = int(sec[10]&0x1f)<<8 | int(sec[11]) // first program's PMT PID
		break
	}
	if pmtPID < 0 {
		t.Fatal("no PAT in generated TS")
	}
	desc := []byte{0xB0, 5, 1, 0, 0x0A, 0x35, 0x00} // version 1.0; profile 5, level 6, rpu 1, el 0, bl 1; compat id 0
	for off := 0; off+pkt <= len(ts); off += pkt {
		p := ts[off : off+pkt]
		if pid := int(p[1]&0x1f)<<8 | int(p[2]); pid != pmtPID || p[1]&0x40 == 0 {
			continue
		}
		start := 5 + int(p[4])
		sec := p[start:]
		secLen := int(sec[1]&0x0f)<<8 | int(sec[2])
		section := sec[:3+secLen] // includes the CRC
		progInfoLen := int(section[10]&0x0f)<<8 | int(section[11])
		pos := 12 + progInfoLen
		for pos+5 <= len(section)-4 {
			esLen := int(section[pos+3]&0x0f)<<8 | int(section[pos+4])
			if section[pos] != 0x24 { // not HEVC
				pos += 5 + esLen
				continue
			}
			esEnd := pos + 5 + esLen
			body := append([]byte{}, section[:esEnd]...)
			body = append(body, desc...)
			body = append(body, section[esEnd:len(section)-4]...)
			newLen := len(body) + 4 - 3
			body[1] = body[1]&0xf0 | byte(newLen>>8)
			body[2] = byte(newLen)
			nl := esLen + len(desc)
			body[pos+3] = body[pos+3]&0xf0 | byte(nl>>8)
			body[pos+4] = byte(nl)
			crc := mpegCRC(body)
			body = append(body, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
			if start+len(body) > pkt {
				t.Fatal("PMT does not fit in one packet after the descriptor")
			}
			copy(p[start:], body)
			for i := start + len(body); i < pkt; i++ {
				p[i] = 0xff
			}
			return ts
		}
		t.Fatal("no HEVC entry in PMT")
	}
	t.Fatal("no PMT in generated TS")
	return nil
}

// mpegCRC is the MPEG-2 systems CRC-32 over a PSI section body.
func mpegCRC(b []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, x := range b {
		crc ^= uint32(x) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
