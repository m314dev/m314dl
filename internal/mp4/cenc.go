package mp4

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
)

// Scheme is one of the CENC encryption schemes from ISO/IEC 23001-7. The
// scheme says which AES mode is used and whether a crypt:skip block pattern
// applies.
type Scheme string

const (
	SchemeCENC Scheme = "cenc" // AES-128 CTR mode, whole subsample encrypted
	SchemeCBC1 Scheme = "cbc1" // AES-128 CBC mode, whole subsample encrypted
	SchemeCENS Scheme = "cens" // AES-128 CTR mode with a block pattern
	SchemeCBCS Scheme = "cbcs" // AES-128 CBC mode, block pattern, constant IV
)

// InitInfo holds the protection settings read from an init segment's tenc and
// schm boxes. There is one per track; audio and video each carry their own.
type InitInfo struct {
	Scheme         Scheme
	DefaultKID     [16]byte
	PerSampleIVLen int    // 0, 8 or 16 bytes; 0 means one constant IV (cbcs)
	CryptByteBlock int    // pattern: how many 16-byte blocks in a row are encrypted
	SkipByteBlock  int    // pattern: how many 16-byte blocks after them are left clear
	ConstantIV     []byte // used when PerSampleIVLen == 0
	Protected      bool
}

// isCTR reports whether the scheme uses AES-CTR. The other two use AES-CBC.
func (s Scheme) isCTR() bool { return s == SchemeCENC || s == SchemeCENS }

// ParseInit reads the CENC protection info from a fragmented MP4 (fMP4) init
// segment. It returns (nil, nil) when the segment is not encrypted.
func ParseInit(initSeg []byte) (*InitInfo, error) {
	stsd, ok := find(initSeg, "moov", "trak", "mdia", "minf", "stbl", "stsd")
	if !ok {
		return nil, nil
	}
	_, _, entries, err := fullBox(stsd.payload)
	if err != nil {
		return nil, err
	}
	// stsd payload: a 4-byte entry_count, then the sample entries
	if len(entries) < 4 {
		return nil, nil
	}
	var info *InitInfo
	walk(entries[4:], func(se box) bool {
		if se.typ != "encv" && se.typ != "enca" {
			return true
		}
		child := sampleEntryChildren(se)
		if child == nil {
			return true
		}
		sinf, ok := find(child, "sinf")
		if !ok {
			return true
		}
		ii, err := parseSinf(sinf)
		if err == nil && ii != nil {
			info = ii
			return false
		}
		return true
	})
	return info, nil
}

func parseSinf(sinf box) (*InitInfo, error) {
	info := &InitInfo{}
	var haveScheme, haveTenc bool
	sinf.children(func(b box) bool {
		switch b.typ {
		case "schm":
			_, _, rest, err := fullBox(b.payload)
			if err == nil && len(rest) >= 4 {
				info.Scheme = Scheme(rest[0:4])
				haveScheme = true
			}
		case "schi":
			b.children(func(c box) bool {
				if c.typ == "tenc" {
					if err := parseTenc(c, info); err == nil {
						haveTenc = true
					}
					return false
				}
				return true
			})
		}
		return true
	})
	if !haveTenc {
		return nil, errors.New("mp4: no tenc box")
	}
	if !haveScheme {
		info.Scheme = SchemeCENC // the spec's default when there is no schm box
	}
	return info, nil
}

// parseTenc reads the TrackEncryptionBox (tenc, ISO 23001-7 §8.2), which
// holds the track's default encryption settings.
func parseTenc(b box, info *InitInfo) error {
	version, _, rest, err := fullBox(b.payload)
	if err != nil {
		return err
	}
	// Layout of rest: reserved(1), then one byte that is reserved in version 0
	// and holds the crypt/skip nibbles in version 1, isProtected(1),
	// perSampleIVSize(1), KID(16). When the IV size is 0 this is followed by
	// constIVsize(1) and constIV(n).
	if len(rest) < 20 {
		return errors.New("mp4: short tenc")
	}
	if version > 0 {
		info.CryptByteBlock = int(rest[1] >> 4)
		info.SkipByteBlock = int(rest[1] & 0x0f)
	}
	info.Protected = rest[2] == 1
	info.PerSampleIVLen = int(rest[3])
	copy(info.DefaultKID[:], rest[4:20])
	if info.Protected && info.PerSampleIVLen == 0 {
		if len(rest) < 21 {
			return errors.New("mp4: tenc missing constant IV size")
		}
		n := int(rest[20])
		if len(rest) < 21+n {
			return errors.New("mp4: tenc truncated constant IV")
		}
		info.ConstantIV = append([]byte(nil), rest[21:21+n]...)
	}
	return nil
}

// subsample is one piece of a sample: a run of clear bytes followed by a run
// of encrypted bytes.
type subsample struct {
	clear     uint32
	encrypted uint32
}

// DecryptFragment decrypts a fragmented MP4 media segment in place with key.
// A segment is an optional styp box followed by moof/mdat pairs (the moof
// holds the sample table, the mdat holds the sample bytes). info comes from
// the track's init segment. The buffer is changed: encrypted sample bytes are
// overwritten with plaintext.
func DecryptFragment(frag []byte, info *InitInfo, key []byte) error {
	if len(key) != 16 {
		return fmt.Errorf("mp4: CENC key is %d bytes, want 16", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	// Walk the top-level boxes. Remember each moof, then decrypt when the mdat
	// that follows it arrives.
	var moofOff int64 = -1
	var moof box
	var perr error
	walk(frag, func(b box) bool {
		switch b.typ {
		case "moof":
			moof = b
			moofOff = b.off
		case "mdat":
			if moofOff < 0 {
				return true // an mdat with no moof before it; skip it
			}
			mdatData := frag[b.off+b.hdrLen : b.off+b.hdrLen+int64(len(b.payload))]
			if err := decryptTrafs(frag, moof, moofOff, b.off, mdatData, info, block); err != nil {
				perr = err
				return false
			}
			moofOff = -1
		}
		return true
	})
	return perr
}

// decryptTrafs decrypts the samples of every traf (track fragment) in one
// moof. The sample bytes live in mdatData.
func decryptTrafs(frag []byte, moof box, moofOff, mdatOff int64, mdatData []byte, info *InitInfo, block cipher.Block) error {
	var ferr error
	moof.children(func(tf box) bool {
		if tf.typ != "traf" {
			return true
		}
		if err := decryptOneTraf(frag, tf, moofOff, mdatOff, mdatData, info, block); err != nil {
			ferr = err
			return false
		}
		return true
	})
	return ferr
}

// trunRun is what one trun (track run) adds to a traf: its sample sizes and
// where its sample data begins. The samples of one trun sit next to each other
// in the mdat. A traf may hold several truns, and their data can sit in
// separate, non-adjacent parts of the mdat. senc and saiz, on the other hand,
// list every sample of the traf as one list in trun order.
type trunRun struct {
	sizes   []uint32
	dataOff int64
	hasOff  bool // data_offset flag (0x000001) present
}

func decryptOneTraf(frag []byte, traf box, moofOff, mdatOff int64, mdatData []byte, info *InitInfo, block cipher.Block) error {
	var (
		truns               []trunRun
		sencIVs             [][]byte
		sencSubs            [][]subsample
		haveSenc            bool
		saizDefault         uint8
		saizSizes           []uint8
		haveSaiz            bool
		saioOffsets         []int64
		baseIsMoof          bool
		tfhdBaseDataOff     int64
		haveTfhdBaseDataOff bool
		tfhdDefaultSize     uint32
		haveTfhdDefaultSize bool
	)
	traf.children(func(b box) bool {
		switch b.typ {
		case "tfhd":
			baseIsMoof, tfhdBaseDataOff, haveTfhdBaseDataOff, tfhdDefaultSize, haveTfhdDefaultSize = parseTfhd(b)
		case "trun":
			// Keep every trun, in order. senc and saiz number the samples across
			// all truns of the traf, so each one is needed to line them up.
			if sizes, off, ok := parseTrunWithDefault(b, tfhdDefaultSize, haveTfhdDefaultSize); ok {
				hasOff := len(b.payload) >= 4 && b.payload[3]&0x01 != 0
				truns = append(truns, trunRun{sizes: sizes, dataOff: off, hasOff: hasOff})
			}
		case "senc":
			sencIVs, sencSubs, haveSenc = parseSencAuto(b.payload, info.PerSampleIVLen)
		case "uuid":
			// The PIFF 1.1 SampleEncryptionBox is the older form of senc. After
			// the 16-byte extended type it has the same layout as senc, plus
			// optional AlgorithmID/IV_size/KID overrides.
			if isUUID(b, uuidPiffSenc) {
				sencIVs, sencSubs, haveSenc = parsePiffSenc(b, info.PerSampleIVLen)
			}
		case "saiz":
			saizDefault, saizSizes, haveSaiz = parseSaiz(b)
		case "saio":
			saioOffsets = parseSaio(b)
		}
		return true
	})
	if len(truns) == 0 {
		return nil // no samples described; nothing to do
	}

	// Work out the base that trun data offsets are measured from, as a position
	// in the whole fragment buffer.
	var base int64
	switch {
	case baseIsMoof:
		base = moofOff
	case haveTfhdBaseDataOff:
		base = tfhdBaseDataOff
	default:
		base = moofOff // no flag set: measure from the moof, the usual fMP4 layout
	}
	mdatBase := mdatOff + mdatHeaderLen(frag, mdatOff)

	// senc and saiz describe all samples of the traf as one list in trun order,
	// so count them all.
	total := 0
	for _, t := range truns {
		total += len(t.sizes)
	}

	// Get each sample's IV and subsample layout. Use senc when present,
	// otherwise the aux info that saiz/saio point to.
	ivs := sencIVs
	subs := sencSubs
	if !haveSenc {
		var err error
		ivs, subs, err = readAuxInfo(frag, moofOff, saioOffsets, saizDefault, saizSizes, haveSaiz, total, info)
		if err != nil {
			return err
		}
	}

	// Visit the samples in trun order, then sample order within each trun. A
	// trun's data begins at base + its data_offset and its samples follow one
	// another from there. A trun with no data_offset continues where the
	// previous one ended.
	i := 0
	var pos int64
	iv := make([]byte, 16) // reused per sample; decryptSample copies it first
	for k, t := range truns {
		if k == 0 || t.hasOff {
			pos = base + t.dataOff - mdatBase
		}
		if pos < 0 || pos > int64(len(mdatData)) {
			return fmt.Errorf("mp4: sample data offset %d outside mdat (len %d)", pos, len(mdatData))
		}
		for _, sz := range t.sizes {
			end := pos + int64(sz)
			if end > int64(len(mdatData)) {
				return fmt.Errorf("mp4: sample %d overruns mdat", i)
			}
			sample := mdatData[pos:end]
			fillSampleIV(iv, ivs, i, info)
			var ss []subsample
			if i < len(subs) {
				ss = subs[i]
			}
			if err := decryptSample(sample, iv, ss, info, block); err != nil {
				return fmt.Errorf("mp4: sample %d: %w", i, err)
			}
			pos = end
			i++
		}
	}
	return nil
}

// fillSampleIV writes the 16-byte IV for sample i into iv. (In CTR mode the IV
// is the starting counter; in CBC mode it starts the chain.) The same buffer
// is reused for every sample; the cipher constructors in decryptSample copy
// it, so that is safe.
func fillSampleIV(iv []byte, ivs [][]byte, i int, info *InitInfo) {
	for j := range iv {
		iv[j] = 0
	}
	if i < len(ivs) && len(ivs[i]) > 0 {
		copy(iv, ivs[i]) // an 8-byte IV fills the first 8 bytes; the rest stay zero
		return
	}
	if len(info.ConstantIV) > 0 {
		copy(iv, info.ConstantIV)
	}
}

// decryptSample decrypts one sample in place, following the scheme in info.
func decryptSample(sample, iv []byte, subs []subsample, info *InitInfo, block cipher.Block) error {
	if info.Scheme.isCTR() {
		return decryptCTR(sample, iv, subs, info, block)
	}
	return decryptCBC(sample, iv, subs, info, block)
}

// decryptCTR handles cenc (no pattern) and cens (pattern). Only the encrypted
// bytes of each subsample use up keystream; clear bytes are left as they are
// and do not advance the stream.
func decryptCTR(sample, iv []byte, subs []subsample, info *InitInfo, block cipher.Block) error {
	stream := cipher.NewCTR(block, iv)
	patterned := info.Scheme == SchemeCENS && (info.CryptByteBlock > 0 || info.SkipByteBlock > 0)
	crypt := func(buf []byte) {
		if !patterned {
			stream.XORKeyStream(buf, buf)
			return
		}
		applyPatternCTR(buf, stream, info.CryptByteBlock, info.SkipByteBlock)
	}
	if len(subs) == 0 {
		crypt(sample)
		return nil
	}
	pos := 0
	for _, s := range subs {
		pos += int(s.clear) // skip the clear bytes; they use no keystream
		e := pos + int(s.encrypted)
		if e > len(sample) {
			return errors.New("subsample exceeds sample")
		}
		crypt(sample[pos:e])
		pos = e
	}
	return nil
}

// decryptCBC handles cbc1 (every block encrypted) and cbcs (block pattern, and
// the same constant IV restarts the chain for each subsample).
func decryptCBC(sample, iv []byte, subs []subsample, info *InitInfo, block cipher.Block) error {
	patterned := info.Scheme == SchemeCBCS && (info.CryptByteBlock > 0 || info.SkipByteBlock > 0)
	decRange := func(buf []byte) {
		if !patterned {
			decryptCBCFull(buf, iv, block)
			return
		}
		applyPatternCBC(buf, iv, block, info.CryptByteBlock, info.SkipByteBlock)
	}
	if len(subs) == 0 {
		decRange(sample)
		return nil
	}
	pos := 0
	for _, s := range subs {
		pos += int(s.clear)
		e := pos + int(s.encrypted)
		if e > len(sample) {
			return errors.New("subsample exceeds sample")
		}
		decRange(sample[pos:e])
		pos = e
	}
	return nil
}

// decryptCBCFull decrypts all whole 16-byte blocks. A partial block at the end
// is left as it is.
func decryptCBCFull(buf, iv []byte, block cipher.Block) {
	n := len(buf) - len(buf)%16
	if n == 0 {
		return
	}
	cipher.NewCBCDecrypter(block, iv[:16]).CryptBlocks(buf[:n], buf[:n])
}

// applyPatternCBC decrypts with the cbcs crypt:skip block pattern: cryptBlocks
// encrypted 16-byte blocks, then skipBlocks clear ones, repeating. The CBC
// chain runs on across the encrypted blocks only; skipped blocks are not
// touched. The chain starts from the given constant IV for this range.
func applyPatternCBC(buf, iv []byte, block cipher.Block, cryptBlocks, skipBlocks int) {
	dec := cipher.NewCBCDecrypter(block, iv[:16])
	// skip=0 means nothing is skipped: every 16-byte block is encrypted and
	// chained, so this is plain CBC over the whole range. Decrypt all whole
	// blocks; a partial block at the end stays clear, as cbcs requires. An
	// earlier version decrypted only the first crypt run (cryptBlocks*16 bytes)
	// and then stopped, which left the rest of every sample encrypted for a
	// (1,0) pattern. A (1,9) pattern was not affected.
	if skipBlocks == 0 {
		n := len(buf) - len(buf)%16
		if n > 0 {
			dec.CryptBlocks(buf[:n], buf[:n])
		}
		return
	}
	unit := (cryptBlocks + skipBlocks) * 16
	for pos := 0; pos+16 <= len(buf); pos += unit {
		cn := cryptBlocks * 16
		if pos+cn > len(buf) {
			cn = (len(buf) - pos) / 16 * 16
		}
		if cn > 0 {
			dec.CryptBlocks(buf[pos:pos+cn], buf[pos:pos+cn])
		}
	}
}

// applyPatternCTR decrypts with the cens crypt:skip pattern using one CTR
// stream. Skipped blocks are jumped over and use no keystream.
func applyPatternCTR(buf []byte, stream cipher.Stream, cryptBlocks, skipBlocks int) {
	unit := (cryptBlocks + skipBlocks) * 16
	pos := 0
	for pos < len(buf) {
		cn := cryptBlocks * 16
		if pos+cn > len(buf) {
			cn = len(buf) - pos
		}
		if cn > 0 {
			stream.XORKeyStream(buf[pos:pos+cn], buf[pos:pos+cn])
		}
		pos += unit
	}
}

// ---- box field parsers ----

func parseTfhd(b box) (baseIsMoof bool, baseDataOff int64, haveBaseDataOff bool, defaultSize uint32, haveDefaultSize bool) {
	_, flags, rest, err := fullBox(b.payload)
	if err != nil || len(rest) < 4 {
		return true, 0, false, 0, false
	}
	baseIsMoof = flags&0x020000 != 0
	rest = rest[4:] // skip track_ID
	if flags&0x000001 != 0 {
		if len(rest) < 8 {
			return baseIsMoof, 0, false, 0, false
		}
		baseDataOff = int64(binary.BigEndian.Uint64(rest))
		haveBaseDataOff = true
		rest = rest[8:]
	}
	if flags&0x000002 != 0 { // sample-description-index-present
		if len(rest) < 4 {
			return baseIsMoof, baseDataOff, haveBaseDataOff, 0, false
		}
		rest = rest[4:]
	}
	if flags&0x000008 != 0 { // default-sample-duration-present
		if len(rest) < 4 {
			return baseIsMoof, baseDataOff, haveBaseDataOff, 0, false
		}
		rest = rest[4:]
	}
	if flags&0x000010 != 0 { // default-sample-size-present
		if len(rest) < 4 {
			return baseIsMoof, baseDataOff, haveBaseDataOff, 0, false
		}
		defaultSize = binary.BigEndian.Uint32(rest)
		haveDefaultSize = true
	}
	return baseIsMoof, baseDataOff, haveBaseDataOff, defaultSize, haveDefaultSize
}

// parseTrun reads a trun using only the fields written in the trun itself,
// with no default sample size from the tfhd. It is kept for callers that only
// need those fields; CENC decryption uses parseTrunWithDefault below.
func parseTrun(b box) (sizes []uint32, dataOff int64, ok bool) {
	return parseTrunWithDefault(b, 0, false)
}

func parseTrunWithDefault(b box, defaultSampleSize uint32, haveDefaultSampleSize bool) (sizes []uint32, dataOff int64, ok bool) {
	version, flags, rest, err := fullBox(b.payload)
	if err != nil || len(rest) < 4 {
		return nil, 0, false
	}
	count := binary.BigEndian.Uint32(rest)
	rest = rest[4:]
	if flags&0x000001 != 0 { // data-offset present
		if len(rest) < 4 {
			return nil, 0, false
		}
		dataOff = int64(int32(binary.BigEndian.Uint32(rest)))
		rest = rest[4:]
	}
	if flags&0x000004 != 0 { // first-sample-flags present
		if len(rest) < 4 {
			return nil, 0, false
		}
		rest = rest[4:]
	}
	durPresent := flags&0x000100 != 0
	sizePresent := flags&0x000200 != 0
	flagsPresent := flags&0x000400 != 0
	ctsPresent := flags&0x000800 != 0
	sizes = make([]uint32, 0, count)
	for i := uint32(0); i < count; i++ {
		if durPresent {
			if len(rest) < 4 {
				return nil, 0, false
			}
			rest = rest[4:]
		}
		var sz uint32
		if sizePresent {
			if len(rest) < 4 {
				return nil, 0, false
			}
			sz = binary.BigEndian.Uint32(rest)
			rest = rest[4:]
		} else if haveDefaultSampleSize {
			sz = defaultSampleSize
		}
		if flagsPresent {
			if len(rest) < 4 {
				return nil, 0, false
			}
			rest = rest[4:]
		}
		if ctsPresent {
			if len(rest) < 4 {
				return nil, 0, false
			}
			rest = rest[4:]
		}
		sizes = append(sizes, sz)
	}
	_ = version
	return sizes, dataOff, true
}

// parseSencAuto parses a senc payload using the IV size from the init. If the
// entries do not fill the box exactly with that size, it tries the other
// common size (8 instead of 16, or 16 instead of 8). This matters for Smooth
// Streaming: its init is built by us, and the tenc IV size in it is a guess. A
// wrong guess would decrypt to garbage with no error. The exact-fit check runs
// first; if neither size fits exactly, the declared size is used without the
// exact-fit requirement.
func parseSencAuto(payload []byte, ivLen int) (ivs [][]byte, subs [][]subsample, ok bool) {
	if ivs, subs, ok = parseSencPayload(payload, ivLen, true); ok {
		return ivs, subs, true
	}
	if alt := map[int]int{8: 16, 16: 8}[ivLen]; alt != 0 {
		if ivs, subs, ok = parseSencPayload(payload, alt, true); ok {
			return ivs, subs, true
		}
	}
	return parseSencPayload(payload, ivLen, false)
}

// parsePiffSenc parses the PIFF SampleEncryptionBox (uuid A2394F52-...). After
// the 16-byte extended type it looks like a senc, except that when flag 0x1 is
// set an AlgorithmID(3)/IV_size(1)/KID(16) override comes first, and its IV
// size takes priority over the one from the init.
func parsePiffSenc(b box, ivLen int) ([][]byte, [][]subsample, bool) {
	p := b.payload[16:]
	_, flags, rest, err := fullBox(p)
	if err != nil {
		return nil, nil, false
	}
	if flags&0x000001 != 0 {
		if len(rest) < 20 {
			return nil, nil, false
		}
		ivLen = int(rest[3])
		// rebuild as a plain senc payload: version and flags, then the entries
		p = append(append([]byte(nil), p[:4]...), rest[20:]...)
	}
	return parseSencAuto(p, ivLen)
}

// parseSencPayload reads the senc entries. When exact is set, the entries must
// use up the payload with nothing left over; that check is how an IV-size
// guess is tested.
func parseSencPayload(payload []byte, ivLen int, exact bool) (ivs [][]byte, subs [][]subsample, ok bool) {
	_, flags, rest, err := fullBox(payload)
	if err != nil || len(rest) < 4 {
		return nil, nil, false
	}
	count := binary.BigEndian.Uint32(rest)
	rest = rest[4:]
	useSub := flags&0x000002 != 0
	// ivLen == 0 means a constant IV (cbcs), so the entries carry no per-sample
	// IV.
	ivs = make([][]byte, 0, count)
	subs = make([][]subsample, 0, count)
	for i := uint32(0); i < count; i++ {
		if len(rest) < ivLen {
			return nil, nil, false
		}
		ivs = append(ivs, append([]byte(nil), rest[:ivLen]...))
		rest = rest[ivLen:]
		var ss []subsample
		if useSub {
			if len(rest) < 2 {
				return nil, nil, false
			}
			sc := binary.BigEndian.Uint16(rest)
			rest = rest[2:]
			for j := uint16(0); j < sc; j++ {
				if len(rest) < 6 {
					return nil, nil, false
				}
				ss = append(ss, subsample{
					clear:     uint32(binary.BigEndian.Uint16(rest)),
					encrypted: binary.BigEndian.Uint32(rest[2:]),
				})
				rest = rest[6:]
			}
		}
		subs = append(subs, ss)
	}
	if exact && len(rest) != 0 {
		return nil, nil, false
	}
	return ivs, subs, true
}

func parseSaiz(b box) (defaultSize uint8, sizes []uint8, ok bool) {
	_, flags, rest, err := fullBox(b.payload)
	if err != nil {
		return 0, nil, false
	}
	if flags&0x000001 != 0 {
		if len(rest) < 8 {
			return 0, nil, false
		}
		rest = rest[8:] // aux_info_type and its parameter, 4 bytes each
	}
	if len(rest) < 5 {
		return 0, nil, false
	}
	defaultSize = rest[0]
	count := binary.BigEndian.Uint32(rest[1:])
	rest = rest[5:]
	if defaultSize == 0 {
		if len(rest) < int(count) {
			return 0, nil, false
		}
		sizes = append([]byte(nil), rest[:count]...)
	}
	return defaultSize, sizes, true
}

func parseSaio(b box) []int64 {
	version, flags, rest, err := fullBox(b.payload)
	if err != nil {
		return nil
	}
	if flags&0x000001 != 0 {
		if len(rest) < 8 {
			return nil
		}
		rest = rest[8:]
	}
	if len(rest) < 4 {
		return nil
	}
	count := binary.BigEndian.Uint32(rest)
	rest = rest[4:]
	offs := make([]int64, 0, count)
	for i := uint32(0); i < count; i++ {
		if version == 0 {
			if len(rest) < 4 {
				return offs
			}
			offs = append(offs, int64(binary.BigEndian.Uint32(rest)))
			rest = rest[4:]
		} else {
			if len(rest) < 8 {
				return offs
			}
			offs = append(offs, int64(binary.BigEndian.Uint64(rest)))
			rest = rest[8:]
		}
	}
	return offs
}

// readAuxInfo reads each sample's IV and subsample list from the CENC sample
// auxiliary information. saiz gives the size of each entry and saio says where
// the entries start. This path is used when there is no senc box.
func readAuxInfo(frag []byte, moofOff int64, saioOffsets []int64, saizDefault uint8, saizSizes []uint8, haveSaiz bool, nSamples int, info *InitInfo) ([][]byte, [][]subsample, error) {
	if len(saioOffsets) == 0 || !haveSaiz {
		return nil, nil, nil // no aux info: whole samples encrypted, IV from tenc
	}
	// The saio offset is measured from the start of the moof (the usual
	// default-base-is-moof case).
	auxBase := moofOff + saioOffsets[0]
	perEntry := len(saioOffsets) == nSamples
	ivLen := info.PerSampleIVLen // 0 for a constant IV (cbcs): entry is subsample data only
	ivs := make([][]byte, 0, nSamples)
	subs := make([][]subsample, 0, nSamples)
	cur := auxBase
	for i := 0; i < nSamples; i++ {
		if perEntry {
			cur = moofOff + saioOffsets[i]
		}
		size := int(saizDefault)
		if saizDefault == 0 {
			if i >= len(saizSizes) {
				return nil, nil, errors.New("mp4: saiz shorter than sample count")
			}
			size = int(saizSizes[i])
		}
		if cur < 0 || cur+int64(size) > int64(len(frag)) {
			return nil, nil, fmt.Errorf("mp4: aux info for sample %d out of range", i)
		}
		entry := frag[cur : cur+int64(size)]
		if len(entry) < ivLen {
			return nil, nil, fmt.Errorf("mp4: aux entry %d shorter than IV", i)
		}
		ivs = append(ivs, append([]byte(nil), entry[:ivLen]...))
		var ss []subsample
		if len(entry) > ivLen {
			r := entry[ivLen:]
			if len(r) >= 2 {
				sc := binary.BigEndian.Uint16(r)
				r = r[2:]
				for j := 0; j < int(sc) && len(r) >= 6; j++ {
					ss = append(ss, subsample{
						clear:     uint32(binary.BigEndian.Uint16(r)),
						encrypted: binary.BigEndian.Uint32(r[2:]),
					})
					r = r[6:]
				}
			}
		}
		subs = append(subs, ss)
		if !perEntry {
			cur += int64(size)
		}
	}
	return ivs, subs, nil
}

// mdatHeaderLen returns the header length (8 or 16) of the mdat box at off.
func mdatHeaderLen(frag []byte, off int64) int64 {
	if off+8 > int64(len(frag)) {
		return 8
	}
	if binary.BigEndian.Uint32(frag[off:]) == 1 {
		return 16
	}
	return 8
}
