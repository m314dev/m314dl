// Package sampleaes decrypts HLS SAMPLE-AES elementary streams inside an
// MPEG-2 transport stream, following Apple's "MPEG-2 Stream Encryption Format
// for HTTP Live Streaming".
//
// SAMPLE-AES leaves the segment container alone and encrypts only the media
// samples inside it. Within each elementary stream (one audio or video track
// of the TS) the codec payload is encrypted with AES-128-CBC in a pattern that
// depends on the codec, while headers and structural bytes stay readable. The
// caller supplies only the content key. Everything else, such as which PIDs
// carry which codec and where each sample starts and ends, is read from the TS
// itself.
//
// Two codecs are handled: AAC in ADTS and H.264. An H.264 NAL unit is normally
// "escaped": emulation-prevention (EP) bytes are inserted so that no byte run
// inside it looks like a start code. A SAMPLE-AES encoder encrypts the sample
// in place inside that escaped NAL (the 1-in-10 encrypted blocks sit within the
// emulation-prevented bitstream) and then runs start-code emulation prevention
// again over the encrypted NAL. That adds one more EP layer, covering the new
// 00 00 0x runs the ciphertext produced. Decryption is the exact inverse: strip
// that one EP layer, decrypt the blocks in place, and return the NAL. The
// result is the original clear escaped NAL, unchanged. Its length shrinks only
// because the ciphertext's added EP layer is gone, which is why the video PES
// is re-packetized. No EP is added back. AAC frames carry no emulation
// prevention and keep their length, so they are decrypted fully in place. The
// PMT's encrypted stream_type values are rewritten to their clear equivalents
// so a downstream demuxer treats the result as clear.
//
// The code follows the spec's encryption listings (2-1 for H.264, 2-2 for AAC)
// and was checked against the reference decoders (hls.js, and ffmpeg's
// hls_sample_encryption.c) and against real streams encrypted by Bento4's
// mp42hls. AC-3 and E-AC-3 are not decrypted; an encrypted stream of either
// type is reported as an error instead of being passed through as if it were
// clear. HEVC has no stream_type mapping here at all, so an HEVC stream's
// packets pass through untouched, still encrypted.
package sampleaes

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
)

const (
	pktLen = 188
	sync   = 0x47

	// H.264 SAMPLE-AES pattern (spec Listing 2-1).
	avcLeader = 32  // 1 nal_unit_type byte + 31 unencrypted leader bytes
	avcPeriod = 160 // 16 encrypted + 144 unencrypted
	avcMinNAL = 48  // NAL units <= 48 bytes are not encrypted

	// AAC SAMPLE-AES pattern (spec Listing 2-2): 16 clear bytes after the ADTS
	// header, then whole 16-byte CBC blocks.
	aacLeader = 16
)

type codec int

const (
	codecOther codec = iota
	codecAVC
	codecAAC
	codecAC3  // detected only so Decrypt can report it; not decrypted
	codecEAC3 // detected only so Decrypt can report it; not decrypted
)

// Decrypt decrypts the SAMPLE-AES samples in seg with the 16-byte key and
// 16-byte iv and returns a clean transport stream. A buffer that is not a
// transport stream (no sync bytes at 188-byte spacing) or that carries no
// SAMPLE-AES streams is returned unchanged.
func Decrypt(seg, key, iv []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("sampleaes: key is %d bytes, want 16", len(key))
	}
	if len(iv) != 16 {
		return nil, fmt.Errorf("sampleaes: iv is %d bytes, want 16", len(iv))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	start := firstSync(seg)
	if start < 0 {
		return seg, nil // not a transport stream; nothing to do
	}
	body := seg[start:]
	n := len(body) / pktLen

	// A slot holds output bytes that are ready, or stands in for a video PES
	// whose bytes are filled in once the whole PES has been collected. Slots
	// keep the source order of each item's first packet.
	type slot struct{ data []byte }
	type span struct {
		s   *slot
		off int
		ln  int
	}
	type pesAsm struct {
		pid     uint16
		c       codec
		payload []byte
		slot    *slot  // video: placeholder for the re-packetized PES
		header  []byte // video: first packet's TS header + adaptation field
		spans   []span // audio: where the same-length decrypted payload is written back
	}

	var out []*slot
	emit := func(b []byte) { out = append(out, &slot{data: b}) }

	var (
		pmtPID  uint16
		havePMT bool
		elems   = map[uint16]codec{}
		asm     = map[uint16]*pesAsm{}
	)

	flushAudio := func(a *pesAsm) error {
		es := pesPayload(a.payload)
		if es >= 0 && es < len(a.payload) {
			if err := decryptAudio(a.payload[es:], a.c, block, iv); err != nil {
				return err
			}
		}
		pos := 0
		for _, sp := range a.spans {
			copy(sp.s.data[sp.off:sp.off+sp.ln], a.payload[pos:pos+sp.ln])
			pos += sp.ln
		}
		return nil
	}
	flushVideo := func(a *pesAsm) {
		a.slot.data = packetize(a.pid, a.header, decryptVideoPES(a.payload, block, iv))
	}
	flush := func(id uint16) error {
		a := asm[id]
		if a == nil {
			return nil
		}
		delete(asm, id)
		if a.c == codecAVC {
			flushVideo(a)
			return nil
		}
		return flushAudio(a)
	}

	for i := 0; i < n; i++ {
		p := body[i*pktLen : (i+1)*pktLen]
		if p[0] != sync {
			emit(dup(p))
			continue
		}
		id := pid(p)

		if id == 0 && pusi(p) {
			if pp, ok := patPMTPID(p); ok {
				pmtPID, havePMT = pp, true
			}
			emit(dup(p))
			continue
		}
		if havePMT && id == pmtPID && pusi(p) && len(elems) == 0 {
			m, rewritten, ok := rewritePMT(p)
			if !ok {
				emit(dup(p))
				continue
			}
			if err := unsupportedCodecs(m); err != nil {
				return nil, err
			}
			elems = m
			emit(rewritten)
			continue
		}

		c, isMedia := elems[id]
		off := payloadOffset(p)
		if !isMedia || off >= pktLen {
			// Not a media packet, or a media packet with no payload (PCR only).
			// Pass it through unchanged so its adaptation field and PCR (the
			// program clock reference) survive.
			emit(dup(p))
			continue
		}

		if c == codecAVC {
			if pusi(p) {
				if err := flush(id); err != nil {
					return nil, err
				}
				s := &slot{}
				out = append(out, s)
				// Keep the first packet's TS header and adaptation field (PCR,
				// random-access flag) on the re-packetized PES.
				// ponytail: an adaptation field on a continuation packet of a
				// video PES is dropped. That is uncommon; the PCR usually rides on
				// the packet that starts the PES. The PTS/DTS timestamps in the PES
				// header survive, so players can rebuild the timing. Carry
				// per-packet adaptation fields if a stream needs it.
				a := &pesAsm{pid: id, c: c, slot: s, header: dup(p[:off])}
				a.payload = append(a.payload, p[off:]...)
				asm[id] = a
			} else if a := asm[id]; a != nil {
				a.payload = append(a.payload, p[off:]...)
			} else {
				emit(dup(p)) // continuation with no start seen; cannot reassemble
			}
			continue
		}

		// Audio: emit the packet now and decrypt it in place at flush time. The
		// length does not change.
		emit(dup(p))
		s := out[len(out)-1]
		if pusi(p) {
			if err := flush(id); err != nil {
				return nil, err
			}
			a := &pesAsm{pid: id, c: c}
			a.payload = append(a.payload, p[off:]...)
			a.spans = append(a.spans, span{s, off, pktLen - off})
			asm[id] = a
		} else if a := asm[id]; a != nil {
			a.payload = append(a.payload, p[off:]...)
			a.spans = append(a.spans, span{s, off, pktLen - off})
		}
	}
	for id := range asm {
		if err := flush(id); err != nil {
			return nil, err
		}
	}
	if !havePMT {
		return seg, nil // no PMT, so streams cannot be identified; leave untouched
	}

	var buf bytes.Buffer
	buf.Grow(len(body) + 4*pktLen)
	for _, s := range out {
		buf.Write(s.data)
	}
	result := buf.Bytes()
	renumberCC(result)
	return result, nil
}

// firstSync returns the offset of the first of three sync bytes spaced 188
// bytes apart, or -1 if the buffer is not a transport stream.
func firstSync(b []byte) int {
	for i := 0; i+2*pktLen < len(b); i++ {
		if b[i] == sync && b[i+pktLen] == sync && b[i+2*pktLen] == sync {
			return i
		}
	}
	if len(b) >= pktLen && len(b) < 3*pktLen && b[0] == sync {
		return 0
	}
	return -1
}

func pid(p []byte) uint16 { return uint16(p[1]&0x1f)<<8 | uint16(p[2]) }
func pusi(p []byte) bool  { return p[1]&0x40 != 0 }
func afc(p []byte) int    { return int(p[3]>>4) & 0x3 }

// payloadOffset returns where the TS payload begins, or pktLen when the packet
// carries no payload (adaptation-only or reserved).
func payloadOffset(p []byte) int {
	switch afc(p) {
	case 1:
		return 4
	case 3:
		off := 5 + int(p[4])
		if off > pktLen {
			return pktLen
		}
		return off
	default:
		return pktLen
	}
}

func dup(b []byte) []byte { return append([]byte(nil), b...) }

// patPMTPID reads the first program's PMT PID from a PAT packet. The PAT
// (program association table) lists each program and the PID of its PMT
// (program map table). A PAT fits in one packet.
func patPMTPID(p []byte) (uint16, bool) {
	off := payloadOffset(p)
	if off >= pktLen {
		return 0, false
	}
	pl := p[off:]
	ptr := int(pl[0])
	if 1+ptr > len(pl) {
		return 0, false
	}
	sec := pl[1+ptr:]
	if len(sec) < 12 || sec[0] != 0x00 {
		return 0, false
	}
	secLen := int(sec[1]&0x0f)<<8 | int(sec[2])
	end := 3 + secLen
	if end > len(sec) {
		end = len(sec)
	}
	for i := 8; i+4 <= end-4; i += 4 {
		prog := uint16(sec[i])<<8 | uint16(sec[i+1])
		pmt := uint16(sec[i+2]&0x1f)<<8 | uint16(sec[i+3])
		if prog != 0 {
			return pmt, true
		}
	}
	return 0, false
}

// rewritePMT works out each elementary stream's codec and returns a copy of the
// PMT packet with the encrypted stream_type values rewritten to their clear
// equivalents and the section CRC recomputed. A PMT fits in a single packet.
func rewritePMT(p []byte) (map[uint16]codec, []byte, bool) {
	out := dup(p)
	off := payloadOffset(out)
	if off >= pktLen {
		return nil, nil, false
	}
	pl := out[off:]
	ptr := int(pl[0])
	base := off + 1 + ptr // index into out where the section starts
	if base+12 > pktLen || out[base] != 0x02 {
		return nil, nil, false
	}
	secLen := int(out[base+1]&0x0f)<<8 | int(out[base+2])
	end := base + 3 + secLen
	if end > pktLen || end < base+12 {
		return nil, nil, false
	}
	progInfoLen := int(out[base+10]&0x0f)<<8 | int(out[base+11])
	pos := base + 12 + progInfoLen
	elems := map[uint16]codec{}
	for pos+5 <= end-4 {
		stype := out[pos]
		epid := uint16(out[pos+1]&0x1f)<<8 | uint16(out[pos+2])
		esLen := int(out[pos+3]&0x0f)<<8 | int(out[pos+4])
		if clear, c, enc := codecFor(stype); c != codecOther {
			if enc {
				out[pos] = clear
			}
			elems[epid] = c
		}
		pos += 5 + esLen
	}
	binary.BigEndian.PutUint32(out[end-4:end], mpegCRC(out[base:end-4]))
	return elems, out, true
}

// codecFor maps a PMT stream_type to its codec and its clear stream_type. It
// accepts both the SAMPLE-AES encrypted types (spec §3) and the normal types,
// because the manifest already says the stream is SAMPLE-AES and some packagers
// do not remap the stream_type. enc reports whether the type was an encrypted
// one, which means it must be rewritten to clear.
func codecFor(stype byte) (clear byte, c codec, enc bool) {
	switch stype {
	case 0xdb:
		return 0x1b, codecAVC, true
	case 0x1b:
		return 0x1b, codecAVC, false
	case 0xcf:
		return 0x0f, codecAAC, true
	case 0x0f:
		return 0x0f, codecAAC, false
	case 0xc1:
		return 0x81, codecAC3, true
	case 0x81:
		return 0x81, codecAC3, false
	case 0xc2:
		return 0x87, codecEAC3, true
	case 0x87:
		return 0x87, codecEAC3, false
	}
	return 0, codecOther, false
}

// unsupportedCodecs returns an error when the PMT lists an encrypted codec this
// package cannot decrypt. Without this check the stream_type would be rewritten
// to clear while the samples stayed scrambled.
func unsupportedCodecs(elems map[uint16]codec) error {
	for _, c := range elems {
		switch c {
		case codecAC3:
			return fmt.Errorf("sampleaes: encrypted AC-3 audio is not supported")
		case codecEAC3:
			return fmt.Errorf("sampleaes: encrypted E-AC-3 audio is not supported")
		}
	}
	return nil
}

// pesPayload returns the offset of the elementary-stream data within a PES
// packet (just past the PES header), or -1 if the buffer is not a PES packet.
func pesPayload(pes []byte) int {
	if len(pes) < 9 || pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		return -1
	}
	if pes[6]&0xc0 != 0x80 {
		return 6 // no PES header extension (unexpected for audio or video)
	}
	off := 9 + int(pes[8])
	if off > len(pes) {
		return -1
	}
	return off
}

// ---- codec decryption ----

// decryptAudio decrypts each audio frame in an audio PES's elementary stream in
// place. The length does not change: CBC works on whole blocks and audio has no
// emulation prevention.
func decryptAudio(es []byte, c codec, block cipher.Block, iv []byte) error {
	if c != codecAAC {
		return nil // only AAC reaches here (AC-3 is rejected earlier)
	}
	for pos := 0; pos+7 <= len(es); {
		if es[pos] != 0xff || es[pos+1]&0xf0 != 0xf0 {
			break // not an ADTS syncword
		}
		frameLen := int(es[pos+3]&0x03)<<11 | int(es[pos+4])<<3 | int(es[pos+5])>>5
		if frameLen < 7 || pos+frameLen > len(es) {
			break
		}
		decryptAAC(es[pos:pos+frameLen], block, iv)
		pos += frameLen
	}
	return nil
}

// decryptAAC decrypts one ADTS frame in place (spec Listing 2-2).
func decryptAAC(frame []byte, block cipher.Block, iv []byte) {
	hdr := 9
	if frame[1]&0x01 == 1 { // protection_absent: no CRC
		hdr = 7
	}
	raw := len(frame) - hdr
	if raw <= aacLeader {
		return // nothing after the 16-byte leader
	}
	start := hdr + aacLeader
	end := hdr + (raw &^ 15) // whole blocks only; the last raw%16 bytes stay clear
	if end <= start {
		return
	}
	cbc(frame[start:end], block, iv)
}

// decryptVideoPES decrypts the H.264 NAL units in a video PES and returns the
// PES with its new, possibly smaller, length written into the PES_packet_length
// field. A protected NAL shrinks because decryption strips the one EP layer the
// encoder added over the ciphertext (see decryptAVCNAL); nothing is re-escaped.
func decryptVideoPES(pes []byte, block cipher.Block, iv []byte) []byte {
	es := pesPayload(pes)
	if es < 0 || es >= len(pes) {
		return pes
	}
	newES := decryptVideoES(pes[es:], block, iv)
	if len(newES) == len(pes)-es {
		return pes // unchanged length; the original buffer is already correct
	}
	out := make([]byte, 0, es+len(newES))
	out = append(out, pes[:es]...)
	out = append(out, newES...)
	// PES_packet_length counts the bytes after the length field. Video PES
	// packets commonly use 0 (unbounded); keep 0 in that case. Otherwise
	// recompute it, and fall back to 0 if it would not fit in 16 bits.
	if pes[4] != 0 || pes[5] != 0 {
		n := len(out) - 6
		if n > 0xffff {
			n = 0
		}
		binary.BigEndian.PutUint16(out[4:6], uint16(n))
	}
	return out
}

// decryptVideoES walks the Annex-B NAL units in a video elementary stream,
// decrypts the protected ones, and returns the rebuilt stream.
func decryptVideoES(es []byte, block cipher.Block, iv []byte) []byte {
	type sc struct{ pos, prefix int } // pos = index of 00 00 01; prefix = 3 or 4
	var scs []sc
	for i := 0; i+3 <= len(es); {
		if es[i] == 0 && es[i+1] == 0 && es[i+2] == 1 {
			prefix := 3
			if i > 0 && es[i-1] == 0 {
				prefix = 4
			}
			scs = append(scs, sc{i, prefix})
			i += 3
		} else {
			i++
		}
	}
	if len(scs) == 0 {
		return es
	}
	out := make([]byte, 0, len(es)+64)
	out = append(out, es[:scs[0].pos-(scs[0].prefix-3)]...)
	for i, s := range scs {
		// Emit a 3-byte 00 00 01 start-code prefix. A 4-byte prefix in the source
		// (00 00 00 01) is normalized to 3 bytes: both are valid Annex-B
		// delimiters and the extra leading zero carries no NAL data. (The
		// boundary math via s.prefix keeps that leading zero out of the NAL.)
		out = append(out, es[s.pos:s.pos+3]...)
		nalStart := s.pos + 3
		nalEnd := len(es)
		if i+1 < len(scs) {
			nalEnd = scs[i+1].pos - (scs[i+1].prefix - 3)
		}
		if nalEnd < nalStart {
			nalEnd = nalStart
		}
		out = append(out, decryptAVCNAL(es[nalStart:nalEnd], block, iv)...)
	}
	return out
}

// decryptAVCNAL decrypts one H.264 NAL unit (spec Listing 2-1). NAL types other
// than 1/5, and units of 48 bytes or fewer, are returned unchanged.
//
// The emulation-prevention bytes are removed once and never added back.
// SAMPLE-AES encoders apply start-code emulation prevention over the NAL after
// encrypting it (the encrypted blocks sit inside the escaped bitstream).
// Removing one layer recovers the encrypted NAL, and decrypting that gives the
// original clear NAL with its own emulation prevention still in place.
// Re-escaping would escape it twice and corrupt the slice. This matches the
// reference decoders (for example ffmpeg's hls_sample_encryption, which runs
// remove_scep_3_bytes and then decrypts, with no re-escape).
func decryptAVCNAL(nal []byte, block cipher.Block, iv []byte) []byte {
	if len(nal) == 0 {
		return nal
	}
	t := nal[0] & 0x1f
	if (t != 1 && t != 5) || len(nal) <= avcMinNAL {
		return nal
	}
	raw := discardEP(nal)
	dec := cipher.NewCBCDecrypter(block, iv) // the IV restarts for each NAL; this copies iv
	data, rem := avcLeader, len(raw)-avcLeader
	for rem > 0 {
		if rem > 16 {
			dec.CryptBlocks(raw[data:data+16], raw[data:data+16])
			data += 16
			rem -= 16
		}
		skip := 144
		if rem < skip {
			skip = rem
		}
		data += skip
		rem -= skip
	}
	return raw
}

// cbc decrypts buf in place as AES-128-CBC starting from iv. The length of buf
// must be a multiple of 16. iv is not modified.
func cbc(buf []byte, block cipher.Block, iv []byte) {
	if len(buf) == 0 || len(buf)%16 != 0 {
		return
	}
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(buf, buf)
}

// discardEP removes H.264 emulation-prevention bytes: every 00 00 03 becomes
// 00 00, as long as at least one byte follows the 03 (a 00 00 03 at the very
// end of the NAL is left alone). This matches the RBSP extraction that H.264
// decoders use. The removal does not look at the byte after the 03, so a
// 00 00 03 inside encrypted (pseudo-random) data is stripped just the same.
// That keeps the de-escaped length exactly what the encryptor used when it
// worked out the protected regions.
func discardEP(data []byte) []byte {
	out := make([]byte, 0, len(data))
	i := 0
	for i < len(data) {
		if len(data)-i > 3 && data[i] == 0 && data[i+1] == 0 && data[i+2] == 3 {
			out = append(out, 0, 0)
			i += 3 // drop the emulation-prevention byte
		} else {
			out = append(out, data[i])
			i++
		}
	}
	return out
}

// ---- re-packetization ----

// packetize splits a PES payload into 188-byte TS packets on pid. The first
// packet keeps firstHeader (its original TS header and adaptation field, so the
// PCR and random-access flag survive). The last, short packet is padded with
// adaptation-field stuffing. Continuity counters are fixed later by renumberCC.
func packetize(pid uint16, firstHeader, payload []byte) []byte {
	out := make([]byte, 0, len(payload)+2*pktLen)
	pos := 0
	first := true
	for {
		var hdr []byte
		if first {
			hdr = firstHeader
			first = false
		} else {
			hdr = []byte{sync, byte(pid>>8) & 0x1f, byte(pid), 0x10}
		}
		space := pktLen - len(hdr)
		remain := len(payload) - pos
		if remain >= space {
			out = append(out, hdr...)
			out = append(out, payload[pos:pos+space]...)
			pos += space
			if pos >= len(payload) {
				break
			}
			continue
		}
		out = append(out, lastPacket(hdr, payload[pos:])...)
		break
	}
	return out
}

// lastPacket builds a full 188-byte packet from a short remaining payload and
// fills the gap with adaptation-field stuffing.
func lastPacket(hdr, rest []byte) []byte {
	p := make([]byte, 0, pktLen)
	if len(hdr) == 4 && (hdr[3]>>4)&0x3 == 1 {
		// no adaptation field yet: switch to adaptation field plus payload
		// and fill the gap with stuffing.
		afLen := pktLen - 4 - 1 - len(rest) // bytes after the length byte
		p = append(p, hdr[0], hdr[1], hdr[2], (hdr[3]&0x0f)|0x30)
		p = append(p, byte(afLen))
		if afLen >= 1 {
			p = append(p, 0x00) // adaptation flags
			p = append(p, bytes.Repeat([]byte{0xff}, afLen-1)...)
		}
		p = append(p, rest...)
		return p
	}
	// existing adaptation field: extend it with stuffing.
	afLenOrig := int(hdr[4])
	stuffing := pktLen - len(hdr) - len(rest)
	p = append(p, hdr[0], hdr[1], hdr[2], hdr[3])
	p = append(p, byte(afLenOrig+stuffing))
	p = append(p, hdr[5:5+afLenOrig]...)
	p = append(p, bytes.Repeat([]byte{0xff}, stuffing)...)
	p = append(p, rest...)
	return p
}

// renumberCC rewrites each PID's continuity counter into one unbroken sequence
// within the segment, starting from the counter in that PID's first packet. A
// PID that was not modified keeps its continuity with the previous segment, and
// a re-packetized PID becomes consistent.
func renumberCC(ts []byte) {
	next := map[uint16]byte{}
	seen := map[uint16]bool{}
	for off := 0; off+pktLen <= len(ts); off += pktLen {
		if ts[off] != sync {
			return
		}
		p := ts[off : off+pktLen]
		id := pid(p)
		a := afc(p)
		if id == 0x1fff || (a != 1 && a != 3) {
			continue
		}
		if !seen[id] {
			seen[id] = true
			next[id] = p[3] & 0x0f
		}
		p[3] = (p[3] & 0xf0) | (next[id] & 0x0f)
		next[id] = (next[id] + 1) & 0x0f
	}
}

// mpegCRC computes the MPEG-2 systems CRC-32 used in PSI sections.
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
