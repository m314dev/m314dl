package mp4

import "encoding/binary"

// After DecryptFragment has turned the samples into plaintext, the boxes
// around them still say the stream is encrypted. The init labels its sample
// entries encv/enca and carries sinf/tenc/pssh, and each fragment carries
// senc/saiz/saio. A player (or ffmpeg) that trusts those boxes tries to
// "decrypt" the plaintext and shows a black picture. SanitizeInit and
// StripFragmentProtection remove that metadata, so the output is a clean,
// unprotected stream. mp4decrypt does the same thing.

// emitBox puts a box header with a 32-bit size in front of payload. Init and
// fragment boxes are far smaller than 4 GiB, so the 64-bit largesize form is
// never needed here.
func emitBox(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	copy(out[8:], payload)
	return out
}

// rebuildSeq writes out the boxes in buf again, letting fn change each one.
// fn returns (nil, false) to keep the box as it is, (bytes, true) to replace
// it, or (nil, true) to drop it. Parent sizes come out right on their own,
// because each caller wraps the rebuilt children in a fresh emitBox. If buf
// does not parse, the original bytes are returned whole, never a cut-off copy.
func rebuildSeq(buf []byte, fn func(b box) ([]byte, bool)) []byte {
	out := make([]byte, 0, len(buf))
	if err := walk(buf, func(b box) bool {
		if repl, ok := fn(b); ok {
			out = append(out, repl...) // a nil repl drops the box
		} else {
			out = append(out, buf[b.off:b.off+b.hdrLen+int64(len(b.payload))]...)
		}
		return true
	}); err != nil {
		return buf
	}
	return out
}

// StripFragmentProtection removes senc/saiz/saio from every traf, so a
// fragment whose samples are already decrypted is no longer marked as
// encrypted.
//
// Removing those boxes makes the moof smaller, which moves the mdat after it
// to an earlier position. A trun's data_offset is measured from the start of
// the moof (default-base-is-moof), so every trun's data_offset must shrink by
// the number of bytes removed. Otherwise the demuxer reads samples from the
// wrong place and the picture is garbage.
func StripFragmentProtection(frag []byte) []byte {
	return rebuildSeq(frag, func(b box) ([]byte, bool) {
		if b.typ != "moof" {
			return nil, false
		}
		removed := removedProtectionBytes(b.payload)
		if removed == 0 {
			return nil, false // nothing to strip; keep the moof byte for byte
		}
		return emitBox("moof", rebuildSeq(b.payload, func(c box) ([]byte, bool) {
			if c.typ != "traf" {
				return nil, false
			}
			return emitBox("traf", rebuildSeq(c.payload, func(d box) ([]byte, bool) {
				switch {
				case isProtectionBox(d):
					return nil, true // drop
				case d.typ == "trun":
					return patchTrunDataOffset(d, removed), true
				}
				return nil, false
			})), true
		})), true
	})
}

// removedProtectionBytes adds up the on-disk size of the senc/saiz/saio boxes
// in a moof. That is how far the mdat moves once they are gone.
func removedProtectionBytes(moofPayload []byte) int {
	total := 0
	walk(moofPayload, func(tf box) bool {
		if tf.typ != "traf" {
			return true
		}
		walk(tf.payload, func(d box) bool {
			if isProtectionBox(d) {
				total += int(d.hdrLen) + len(d.payload)
			}
			return true
		})
		return true
	})
	return total
}

// isProtectionBox reports whether b is one of the per-fragment encryption
// boxes: senc, the saiz/saio boxes that locate its aux info, or the PIFF uuid
// form of senc.
func isProtectionBox(b box) bool {
	switch b.typ {
	case "senc", "saiz", "saio":
		return true
	}
	return isUUID(b, uuidPiffSenc)
}

// patchTrunDataOffset subtracts removed from a trun's data_offset field. The
// field exists only when the data-offset flag is set, which is the usual case
// in fragmented MP4. Layout: version+flags(4), sample_count(4),
// data_offset(4, signed).
func patchTrunDataOffset(trun box, removed int) []byte {
	p := append([]byte(nil), trun.payload...)
	if len(p) < 12 {
		return emitBox("trun", p)
	}
	flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	if flags&0x000001 == 0 {
		return emitBox("trun", p) // no data_offset field to fix
	}
	off := int32(binary.BigEndian.Uint32(p[8:12]))
	binary.BigEndian.PutUint32(p[8:12], uint32(off-int32(removed)))
	return emitBox("trun", p)
}

// SanitizeInit removes the protection from an init segment. Each encv/enca
// sample entry gets back its original codec type (the four-character code
// stored in sinf's frma box) and loses its sinf box, and every pssh box is
// dropped. Everything else passes through untouched.
func SanitizeInit(init []byte) []byte {
	return rebuildSeq(init, func(b box) ([]byte, bool) {
		if b.typ != "moov" {
			return nil, false
		}
		return emitBox("moov", rebuildSeq(b.payload, func(c box) ([]byte, bool) {
			switch c.typ {
			case "pssh":
				return nil, true // drop the protection system header
			case "trak":
				return emitBox("trak", sanitizeTrak(c.payload)), true
			}
			return nil, false
		})), true
	})
}

func sanitizeTrak(trak []byte) []byte {
	return rebuildSeq(trak, func(b box) ([]byte, bool) {
		if b.typ != "mdia" {
			return nil, false
		}
		return emitBox("mdia", rebuildSeq(b.payload, func(c box) ([]byte, bool) {
			if c.typ != "minf" {
				return nil, false
			}
			return emitBox("minf", rebuildSeq(c.payload, func(d box) ([]byte, bool) {
				if d.typ != "stbl" {
					return nil, false
				}
				return emitBox("stbl", rebuildSeq(d.payload, func(e box) ([]byte, bool) {
					if e.typ != "stsd" {
						return nil, false
					}
					return sanitizeStsd(e.payload), true
				})), true
			})), true
		})), true
	})
}

// sanitizeStsd removes the protection from each sample entry. stsd is a
// FullBox, so version+flags (4 bytes) and entry_count (4 bytes) come before
// the entries.
func sanitizeStsd(stsd []byte) []byte {
	if len(stsd) < 8 {
		return emitBox("stsd", stsd)
	}
	prefix := stsd[:8]
	entries := rebuildSeq(stsd[8:], func(b box) ([]byte, bool) {
		if b.typ != "encv" && b.typ != "enca" {
			return nil, false
		}
		return deprotectSampleEntry(b), true
	})
	return emitBox("stsd", append(append([]byte(nil), prefix...), entries...))
}

// deprotectSampleEntry rewrites an encv/enca entry as its original codec,
// read from the frma box inside sinf, and removes the sinf box. If there is no
// frma, it leaves the entry as it is instead of guessing a codec.
func deprotectSampleEntry(b box) []byte {
	children := sampleEntryChildren(b)
	if children == nil {
		return emitBox(b.typ, b.payload)
	}
	prefix := b.payload[:len(b.payload)-len(children)]

	orig := ""
	newChildren := rebuildSeq(children, func(c box) ([]byte, bool) {
		if c.typ != "sinf" {
			return nil, false
		}
		if f, ok := find(c.payload, "frma"); ok && len(f.payload) >= 4 {
			orig = string(f.payload[:4])
		}
		return nil, true // drop sinf
	})
	if orig == "" {
		return emitBox(b.typ, b.payload) // no frma; do not risk a wrong codec label
	}
	return emitBox(orig, append(append([]byte(nil), prefix...), newChildren...))
}
