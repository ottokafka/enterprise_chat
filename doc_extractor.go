package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// .doc file extractor (text + image)
// ─── OLE2 / Compound File Binary Format constants ────────────────────────────

const (
	sectorSize = 512
	endOfChain = 0xFFFFFFFE
	freeSect   = 0xFFFFFFFF
	fatSect    = 0xFFFFFFFD
	difsect    = 0xFFFFFFFC
	maxRegSect = 0xFFFFFFFA
)

// CFB header (first 512 bytes of the file)
type cfbHeader struct {
	Magic             [8]byte
	CLSID             [16]byte
	MinorVersion      uint16
	MajorVersion      uint16
	ByteOrder         uint16
	SectorSizePow     uint16
	MiniSectorSizePow uint16
	Reserved          [6]byte
	NumDirSectors     uint32
	NumFATSectors     uint32
	FirstDirSector    uint32
	TransactionSig    uint32
	MiniStreamCutoff  uint32
	FirstMiniFATSect  uint32
	NumMiniFATSects   uint32
	FirstDIFATSect    uint32
	NumDIFATSects     uint32
	DIFAT             [109]uint32
}

// Directory entry (128 bytes each)
type dirEntry struct {
	Name      [32]uint16
	NameLen   uint16
	ObjType   uint8
	ColorFlag uint8
	LeftSib   uint32
	RightSib  uint32
	Child     uint32
	CLSID     [16]byte
	StateBits uint32
	Created   uint64
	Modified  uint64
	StartSect uint32
	SizeLow   uint32
	SizeHigh  uint32
}

// CFB holds the parsed compound file
type CFB struct {
	f          *os.File
	hdr        cfbHeader
	sectorSize uint32
	fat        []uint32
	miniFAT    []uint32
	dirs       []dirEntry
	miniStream []byte
}

func openCFB(path string) (*CFB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	c := &CFB{f: f}

	if err := binary.Read(f, binary.LittleEndian, &c.hdr); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	magic := []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
	for i, b := range magic {
		if c.hdr.Magic[i] != b {
			return nil, fmt.Errorf("not a valid OLE2/CFB file")
		}
	}

	c.sectorSize = 1 << c.hdr.SectorSizePow

	if err := c.readFAT(); err != nil {
		return nil, fmt.Errorf("read FAT: %w", err)
	}
	if err := c.readDirectories(); err != nil {
		return nil, fmt.Errorf("read dirs: %w", err)
	}
	if err := c.readMiniStream(); err != nil {
		return nil, fmt.Errorf("read mini stream: %w", err)
	}

	return c, nil
}

// openCFBFromBytes writes data to a temp file and opens it as a CFB.
// Returns the CFB and a cleanup function; call cleanup() when done (it closes
// cfb.f and removes the temp file).
func openCFBFromBytes(data []byte) (*CFB, func(), error) {
	tmp, err := os.CreateTemp("", "doc-*.doc")
	if err != nil {
		return nil, nil, fmt.Errorf("openCFBFromBytes: create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, nil, fmt.Errorf("openCFBFromBytes: write temp: %w", err)
	}
	tmp.Close()
	cfb, err := openCFB(tmpName)
	if err != nil {
		os.Remove(tmpName)
		return nil, nil, err
	}
	cleanup := func() {
		cfb.f.Close()
		os.Remove(tmpName)
	}
	return cfb, cleanup, nil
}

func (c *CFB) sectorOffset(sect uint32) int64 {
	return int64(sectorSize) + int64(sect)*int64(c.sectorSize)
}

func (c *CFB) readSector(sect uint32) ([]byte, error) {
	buf := make([]byte, c.sectorSize)
	if _, err := c.f.ReadAt(buf, c.sectorOffset(sect)); err != nil {
		return nil, err
	}
	return buf, nil
}

func (c *CFB) readFAT() error {
	var fatSectors []uint32
	for _, s := range c.hdr.DIFAT {
		if s <= maxRegSect {
			fatSectors = append(fatSectors, s)
		}
	}

	difatNext := c.hdr.FirstDIFATSect
	for difatNext <= maxRegSect {
		buf, err := c.readSector(difatNext)
		if err != nil {
			return err
		}
		for i := 0; i < int(c.sectorSize/4)-1; i++ {
			s := binary.LittleEndian.Uint32(buf[i*4:])
			if s <= maxRegSect {
				fatSectors = append(fatSectors, s)
			}
		}
		difatNext = binary.LittleEndian.Uint32(buf[c.sectorSize-4:])
	}

	for _, s := range fatSectors {
		buf, err := c.readSector(s)
		if err != nil {
			return err
		}
		for i := 0; i < int(c.sectorSize/4); i++ {
			c.fat = append(c.fat, binary.LittleEndian.Uint32(buf[i*4:]))
		}
	}
	return nil
}

func (c *CFB) chainRead(startSect uint32, size uint32) ([]byte, error) {
	var result []byte
	sect := startSect
	for sect <= maxRegSect {
		if sect >= uint32(len(c.fat)) {
			break
		}
		buf, err := c.readSector(sect)
		if err != nil {
			return nil, err
		}
		result = append(result, buf...)
		sect = c.fat[sect]
	}
	if size > 0 && size < uint32(len(result)) {
		result = result[:size]
	}
	return result, nil
}

func (c *CFB) readDirectories() error {
	data, err := c.chainRead(c.hdr.FirstDirSector, 0)
	if err != nil {
		return err
	}
	entrySize := 128
	for i := 0; i+entrySize <= len(data); i += entrySize {
		var de dirEntry
		r := strings.NewReader(string(data[i : i+entrySize]))
		if err2 := binary.Read(r, binary.LittleEndian, &de); err2 != nil {
			break
		}
		c.dirs = append(c.dirs, de)
	}
	return nil
}

func (c *CFB) readMiniStream() error {
	if len(c.dirs) == 0 {
		return nil
	}
	root := c.dirs[0]
	if root.StartSect > maxRegSect {
		return nil
	}
	data, err := c.chainRead(root.StartSect, root.SizeLow)
	if err != nil {
		return err
	}
	c.miniStream = data

	sect := c.hdr.FirstMiniFATSect
	for sect <= maxRegSect {
		buf, err2 := c.readSector(sect)
		if err2 != nil {
			break
		}
		for i := 0; i < int(c.sectorSize/4); i++ {
			c.miniFAT = append(c.miniFAT, binary.LittleEndian.Uint32(buf[i*4:]))
		}
		if sect >= uint32(len(c.fat)) {
			break
		}
		sect = c.fat[sect]
	}
	return nil
}

func (c *CFB) miniChainRead(startSect uint32, size uint32) ([]byte, error) {
	miniSectorSize := uint32(1 << c.hdr.MiniSectorSizePow)
	var result []byte
	sect := startSect
	for sect <= maxRegSect && sect < uint32(len(c.miniFAT)) {
		off := sect * miniSectorSize
		if int(off)+int(miniSectorSize) > len(c.miniStream) {
			break
		}
		result = append(result, c.miniStream[off:off+miniSectorSize]...)
		sect = c.miniFAT[sect]
	}
	if size > 0 && size < uint32(len(result)) {
		result = result[:size]
	}
	return result, nil
}

func dirName(de dirEntry) string {
	n := int(de.NameLen/2) - 1
	if n < 0 || n > 32 {
		n = 0
	}
	return string(utf16.Decode(de.Name[:n]))
}

func (c *CFB) findStream(name string) (*dirEntry, bool) {
	for i := range c.dirs {
		de := &c.dirs[i]
		if de.ObjType == 2 && strings.EqualFold(dirName(*de), name) {
			return de, true
		}
	}
	return nil, false
}

func (c *CFB) streamData(de *dirEntry) ([]byte, error) {
	if de.SizeLow < c.hdr.MiniStreamCutoff {
		return c.miniChainRead(de.StartSect, de.SizeLow)
	}
	return c.chainRead(de.StartSect, de.SizeLow)
}

// ─── Text extraction ──────────────────────────────────────────────────────────

func extractText(cfb *CFB) (string, error) {
	wdEntry, ok := cfb.findStream("WordDocument")
	if !ok {
		return "", fmt.Errorf("WordDocument stream not found")
	}
	wdData, err := cfb.streamData(wdEntry)
	if err != nil {
		return "", fmt.Errorf("reading WordDocument stream: %w", err)
	}
	if len(wdData) < 32 {
		return "", fmt.Errorf("WordDocument stream too short")
	}

	wIdent := binary.LittleEndian.Uint16(wdData[0:2])
	if wIdent != 0xA5EC && wIdent != 0xA5DC && wIdent != 0xA59B && wIdent != 0xA5C2 {
		return "", fmt.Errorf("unrecognised FIB magic: 0x%04X", wIdent)
	}

	flags := binary.LittleEndian.Uint16(wdData[0x000A:])
	useOneTable := (flags>>9)&1 == 1
	tableName := "0Table"
	if useOneTable {
		tableName = "1Table"
	}

	if len(wdData) < 0x58 {
		return "", fmt.Errorf("FIB too short")
	}

	ccpText := int32(binary.LittleEndian.Uint32(wdData[0x004C:]))
	if ccpText <= 0 {
		ccpText = int32(binary.LittleEndian.Uint16(wdData[0x004E:]))
	}

	var fcClx, lcbClx uint32
	if len(wdData) >= 0x01AA {
		fcClx = binary.LittleEndian.Uint32(wdData[0x01A2:])
		lcbClx = binary.LittleEndian.Uint32(wdData[0x01A6:])
	}

	tblEntry, ok := cfb.findStream(tableName)
	if !ok {
		if useOneTable {
			tblEntry, ok = cfb.findStream("0Table")
		} else {
			tblEntry, ok = cfb.findStream("1Table")
		}
	}

	if ok && lcbClx > 0 && tblEntry != nil {
		tblData, err2 := cfb.streamData(tblEntry)
		if err2 == nil && int(fcClx)+int(lcbClx) <= len(tblData) {
			clxData := tblData[fcClx : fcClx+lcbClx]
			extracted := extractFromPieceTable(clxData, wdData, ccpText)
			if extracted != "" {
				return extracted, nil
			}
		}
	}

	// Fallback: UTF-16LE scan
	var text strings.Builder
	for i := 0; i+1 < len(wdData); i += 2 {
		ch := binary.LittleEndian.Uint16(wdData[i:])
		switch {
		case ch == 0x000D || ch == 0x0007:
			text.WriteByte('\n')
		case ch == 0x0009:
			text.WriteByte('\t')
		case ch >= 0x0020 && ch < 0xF000:
			text.WriteRune(rune(ch))
		}
	}
	if cleaned := cleanText(text.String()); len(cleaned) > 20 {
		return cleaned, nil
	}

	// Last resort: CP1252 scan
	var sb strings.Builder
	for _, b := range wdData {
		r := cp1252ToRune(b)
		if r >= 0x20 || r == '\n' || r == '\t' || r == '\r' {
			sb.WriteRune(r)
		}
	}
	return cleanText(sb.String()), nil
}

func extractFromPieceTable(clx, wd []byte, ccpText int32) string {
	pos := 0
	for pos < len(clx) {
		if clx[pos] == 2 {
			break
		}
		if clx[pos] == 1 {
			if pos+3 > len(clx) {
				return ""
			}
			cbGrpprl := int(binary.LittleEndian.Uint16(clx[pos+1:]))
			pos += 3 + cbGrpprl
		} else {
			break
		}
	}
	if pos >= len(clx) || clx[pos] != 2 {
		return ""
	}
	pos++

	if pos+4 > len(clx) {
		return ""
	}
	lcbPlcPcd := int(binary.LittleEndian.Uint32(clx[pos:]))
	pos += 4

	if pos+lcbPlcPcd > len(clx) || lcbPlcPcd < 4 {
		return ""
	}
	plcPcd := clx[pos : pos+lcbPlcPcd]

	n := (lcbPlcPcd - 4) / 12
	if n <= 0 {
		return ""
	}

	cpOffsets := make([]uint32, n+1)
	for i := 0; i <= n; i++ {
		if i*4+4 > len(plcPcd) {
			break
		}
		cpOffsets[i] = binary.LittleEndian.Uint32(plcPcd[i*4:])
	}

	pcdBase := (n + 1) * 4
	var sb strings.Builder

	for i := 0; i < n; i++ {
		if pcdBase+i*8+8 > len(plcPcd) {
			break
		}
		pcd := plcPcd[pcdBase+i*8 : pcdBase+i*8+8]

		fcValue := binary.LittleEndian.Uint32(pcd[2:])
		compressed := (fcValue & 0x40000000) != 0
		fc := fcValue &^ 0x40000000
		if compressed {
			fc >>= 1
		}

		cpStart := cpOffsets[i]
		cpEnd := cpOffsets[i+1]
		if cpEnd > uint32(ccpText) {
			cpEnd = uint32(ccpText)
		}
		cpLen := cpEnd - cpStart
		if cpLen == 0 {
			continue
		}

		if compressed {
			end := int(fc) + int(cpLen)
			if end > len(wd) {
				end = len(wd)
			}
			for _, b := range wd[fc:end] {
				r := cp1252ToRune(b)
				switch {
				case r == 0x000D || r == 0x0007:
					sb.WriteByte('\n')
				case r == 0x0009:
					sb.WriteByte('\t')
				case r >= 0x0020:
					sb.WriteRune(r)
				}
			}
		} else {
			byteLen := int(cpLen) * 2
			end := int(fc) + byteLen
			if end > len(wd) {
				end = len(wd)
			}
			chunk := wd[fc:end]
			for j := 0; j+1 < len(chunk); j += 2 {
				ch := binary.LittleEndian.Uint16(chunk[j:])
				switch {
				case ch == 0x000D || ch == 0x0007:
					sb.WriteByte('\n')
				case ch == 0x0009:
					sb.WriteByte('\t')
				case ch >= 0x0020 && ch < 0xF000:
					sb.WriteRune(rune(ch))
				}
			}
		}
	}
	return sb.String()
}

func cp1252ToRune(b byte) rune {
	if b < 0x80 || b > 0x9F {
		return rune(b)
	}
	table := [32]rune{
		0x20AC, 0xFFFD, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
		0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0xFFFD, 0x017D, 0xFFFD,
		0xFFFD, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
		0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0xFFFD, 0x017E, 0x0178,
	}
	return table[b-0x80]
}

func cleanText(s string) string {
	var sb strings.Builder
	prevNL := false
	for _, r := range s {
		if r == '\n' {
			if !prevNL {
				sb.WriteRune(r)
			}
			prevNL = true
		} else {
			prevNL = false
			sb.WriteRune(r)
		}
	}
	return strings.TrimSpace(sb.String())
}

// ─── Image extraction ─────────────────────────────────────────────────────────
//
// Word 97-2003 stores images in two places:
//
//  1. Office Art stream — FIB → fcDggInfo/lcbDggInfo in the Table stream
//     → OfficeArtDggContainer → OfficeArtBStoreContainer → BLIP records.
//     Each BLIP carries a raw JPEG (recType 0xF018/0xF01D) or
//     PNG/DIB (0xF01A/0xF01B) payload.
//
//  2. ObjectPool storage — embedded OLE objects live here as sub-storages
//     containing an \x01Ole10Native stream.
//
//  3. Legacy "Pictures" stream — Word 6/95 style PICF records.
//
//  4. Magic-byte fallback — scan every CFB stream for JPEG/PNG headers.

type extractedImage struct {
	index int
	ext   string
	data  []byte
}

// OfficeArt recType constants
const (
	msofbtBSE             = 0xF007
	msofbtBStoreContainer = 0xF001
	blipJPEG              = 0xF018
	blipJPEGCMYK          = 0xF01D
	blipPNG               = 0xF01A
	blipDIB               = 0xF01B
	blipTIFF              = 0xF01C
	blipEMF               = 0xF01E
	blipWMF               = 0xF01F
)

func isJPEG(t uint16) bool { return t == blipJPEG || t == blipJPEGCMYK }
func isPNG(t uint16) bool  { return t == blipPNG }
func isDIB(t uint16) bool  { return t == blipDIB }

func extractImages(cfb *CFB) ([]extractedImage, error) {
	var images []extractedImage

	// ── 1. Office Art via FIB ────────────────────────────────────────────
	wdEntry, ok := cfb.findStream("WordDocument")
	if ok {
		wdData, err := cfb.streamData(wdEntry)
		if err == nil && len(wdData) >= 0x01E2 {
			flags := binary.LittleEndian.Uint16(wdData[0x000A:])
			useOneTable := (flags>>9)&1 == 1
			tableName := "0Table"
			if useOneTable {
				tableName = "1Table"
			}
			tblEntry, tok := cfb.findStream(tableName)
			if !tok {
				if useOneTable {
					tblEntry, tok = cfb.findStream("0Table")
				} else {
					tblEntry, tok = cfb.findStream("1Table")
				}
			}
			if tok && tblEntry != nil {
				// fcDggInfo @ 0x01DA, lcbDggInfo @ 0x01DE  ([MS-DOC] §2.9.176)
				fcDgg := binary.LittleEndian.Uint32(wdData[0x01DA:])
				lcbDgg := binary.LittleEndian.Uint32(wdData[0x01DE:])
				if lcbDgg > 0 {
					tblData, err2 := cfb.streamData(tblEntry)
					if err2 == nil && int(fcDgg)+int(lcbDgg) <= len(tblData) {
						artData := tblData[fcDgg : fcDgg+lcbDgg]
						imgs := extractBLIPsFromOfficeArt(artData)
						images = append(images, imgs...)
					}
				}
			}
		}
	}

	// ── 2. Legacy "Pictures" stream ──────────────────────────────────────
	if picEntry, ok2 := cfb.findStream("Pictures"); ok2 {
		picData, err2 := cfb.streamData(picEntry)
		if err2 == nil {
			imgs := extractBLIPsFromPictureStream(picData)
			images = append(images, imgs...)
		}
	}

	// ── 3. ObjectPool OLE10Native ────────────────────────────────────────
	images = append(images, extractOLEImages(cfb)...)

	// ── 4. Magic-byte fallback ────────────────────────────────────────────
	if len(images) == 0 {
		images = magicScan(cfb)
	}

	// Re-index sequentially
	for i := range images {
		images[i].index = i + 1
	}
	return images, nil
}

// extractBLIPsFromOfficeArt recursively walks the OfficeArt record tree.
func extractBLIPsFromOfficeArt(data []byte) []extractedImage {
	var out []extractedImage
	walkArtRecords(data, func(recType uint16, recData []byte) {
		ext, payload := blipPayload(recType, recData)
		if payload != nil {
			out = append(out, extractedImage{ext: ext, data: payload})
		}
	})
	return out
}

// walkArtRecords visits every record, recursing into containers (ver == 0xF).
func walkArtRecords(data []byte, fn func(recType uint16, recData []byte)) {
	pos := 0
	for pos+8 <= len(data) {
		verInst := binary.LittleEndian.Uint16(data[pos:])
		recType := binary.LittleEndian.Uint16(data[pos+2:])
		recLen := binary.LittleEndian.Uint32(data[pos+4:])
		pos += 8

		if int(recLen) > len(data)-pos {
			break
		}
		recData := data[pos : pos+int(recLen)]
		pos += int(recLen)

		ver := verInst & 0x000F
		if ver == 0xF {
			walkArtRecords(recData, fn) // container — recurse
		} else {
			fn(recType, recData)
		}
	}
}

// blipPayload extracts raw image bytes from a BLIP or BSE record.
func blipPayload(recType uint16, data []byte) (string, []byte) {
	switch {
	case isJPEG(recType):
		skip := blipHeaderSkip(recType, data)
		if skip < 0 || skip >= len(data) {
			return "", nil
		}
		p := data[skip:]
		if len(p) > 2 && p[0] == 0xFF && p[1] == 0xD8 {
			return "jpg", p
		}

	case isPNG(recType):
		skip := blipHeaderSkip(recType, data)
		if skip < 0 || skip >= len(data) {
			return "", nil
		}
		p := data[skip:]
		if len(p) > 8 && p[0] == 0x89 && p[1] == 'P' && p[2] == 'N' && p[3] == 'G' {
			return "png", p
		}

	case isDIB(recType):
		skip := blipHeaderSkip(recType, data)
		if skip < 0 || skip >= len(data) {
			return "", nil
		}
		dib := data[skip:]
		if len(dib) >= 40 {
			return "bmp", buildBMPFromDIB(dib)
		}

	case recType == msofbtBSE:
		// BSE record layout ([MS-ODRAW] §2.2.32):
		//  [0]     blipTypeWin32
		//  [1]     blipTypeMac
		//  [2-17]  UID
		//  [18-19] tag
		//  [20-23] size
		//  [24-27] cRef
		//  [28-31] foDelay
		//  [32]    usage
		//  [33]    cbName
		//  [34-35] unused
		//  [36+cbName] embedded BLIP record (if inline)
		if len(data) < 36 {
			return "", nil
		}
		cbName := int(data[33])
		blipStart := 36 + cbName
		if blipStart+8 > len(data) {
			return "", nil
		}
		innerType := binary.LittleEndian.Uint16(data[blipStart+2:])
		innerLen := binary.LittleEndian.Uint32(data[blipStart+4:])
		blipEnd := blipStart + 8 + int(innerLen)
		if blipEnd > len(data) {
			blipEnd = len(data)
		}
		return blipPayload(innerType, data[blipStart+8:blipEnd])
	}
	return "", nil
}

// blipHeaderSkip returns how many bytes precede the raw image data in a BLIP.
// BLIP = 16-byte UID [+ 16-byte secondary UID] + 1-byte tag.
// We try both 17 and 33 and pick whichever matches the image magic bytes.
func blipHeaderSkip(recType uint16, data []byte) int {
	for _, skip := range []int{17, 33} {
		if skip >= len(data) {
			continue
		}
		b := data[skip:]
		if len(b) < 2 {
			continue
		}
		if isJPEG(recType) && b[0] == 0xFF && b[1] == 0xD8 {
			return skip
		}
		if isPNG(recType) && len(b) >= 4 && b[0] == 0x89 && b[1] == 'P' {
			return skip
		}
		if isDIB(recType) && len(b) >= 4 {
			sz := binary.LittleEndian.Uint32(b)
			if sz == 40 || sz == 12 || sz == 108 || sz == 124 {
				return skip
			}
		}
	}
	if 17 < len(data) {
		return 17
	}
	return -1
}

// buildBMPFromDIB prepends the 14-byte BITMAPFILEHEADER to a raw DIB.
func buildBMPFromDIB(dib []byte) []byte {
	infoSize := binary.LittleEndian.Uint32(dib[0:4])
	var numColors uint32
	if len(dib) >= 36 {
		numColors = binary.LittleEndian.Uint32(dib[32:36])
	}
	if numColors == 0 && len(dib) >= 16 {
		bitCount := uint32(binary.LittleEndian.Uint16(dib[14:16]))
		if bitCount > 0 && bitCount <= 8 {
			numColors = 1 << bitCount
		}
	}
	dataOffset := 14 + infoSize + numColors*4

	hdr := make([]byte, 14)
	hdr[0], hdr[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(hdr[2:], uint32(14+len(dib)))
	binary.LittleEndian.PutUint32(hdr[10:], dataOffset)
	return append(hdr, dib...)
}

// extractBLIPsFromPictureStream handles the legacy Word 6/95 Pictures stream.
// It scans for JPEG/PNG magic bytes within PICF-prefixed records.
func extractBLIPsFromPictureStream(data []byte) []extractedImage {
	var out []extractedImage
	pos := 0
	for pos+6 <= len(data) {
		cbHeader := int(binary.LittleEndian.Uint16(data[pos:]))
		if cbHeader < 6 || pos+cbHeader > len(data) {
			pos++
			continue
		}
		imgStart := pos + cbHeader
		if imgStart >= len(data) {
			break
		}
		rest := data[imgStart:]
		if img, ok := tryJPEG(rest); ok {
			out = append(out, extractedImage{ext: "jpg", data: img})
			pos = imgStart + len(img)
			continue
		}
		if img, ok := tryPNG(rest); ok {
			out = append(out, extractedImage{ext: "png", data: img})
			pos = imgStart + len(img)
			continue
		}
		pos++
	}
	return out
}

// extractOLEImages reads embedded OLE objects from \x01Ole10Native streams.
func extractOLEImages(cfb *CFB) []extractedImage {
	var out []extractedImage
	for i := range cfb.dirs {
		de := &cfb.dirs[i]
		if de.ObjType != 2 {
			continue
		}
		name := dirName(*de)
		if !strings.EqualFold(name, "\x01Ole10Native") {
			continue
		}
		data, err := cfb.streamData(de)
		if err != nil || len(data) < 4 {
			continue
		}
		payload := data[4:] // skip 4-byte size prefix
		if img, ok := tryJPEG(payload); ok {
			out = append(out, extractedImage{ext: "jpg", data: img})
		} else if img, ok := tryPNG(payload); ok {
			out = append(out, extractedImage{ext: "png", data: img})
		}
	}
	return out
}

// magicScan is the last-resort fallback: scan all CFB streams for image magic bytes.
func magicScan(cfb *CFB) []extractedImage {
	var out []extractedImage
	seen := map[string]bool{}

	for i := range cfb.dirs {
		de := &cfb.dirs[i]
		if de.ObjType != 2 || de.SizeLow < 8 {
			continue
		}
		data, err := cfb.streamData(de)
		if err != nil {
			continue
		}
		for pos := 0; pos+2 <= len(data); pos++ {
			key := fmt.Sprintf("%d-%d", i, pos)
			if seen[key] {
				continue
			}
			if data[pos] == 0xFF && data[pos+1] == 0xD8 {
				if img, ok := tryJPEG(data[pos:]); ok {
					seen[key] = true
					out = append(out, extractedImage{ext: "jpg", data: img})
				}
			}
			if pos+8 <= len(data) &&
				data[pos] == 0x89 && data[pos+1] == 'P' &&
				data[pos+2] == 'N' && data[pos+3] == 'G' {
				if img, ok := tryPNG(data[pos:]); ok {
					seen[key] = true
					out = append(out, extractedImage{ext: "png", data: img})
				}
			}
		}
	}
	return out
}

// tryJPEG extracts a JPEG (FF D8 ... FF D9) from the start of data.
func tryJPEG(data []byte) ([]byte, bool) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, false
	}
	for i := 2; i+1 < len(data); i++ {
		if data[i] == 0xFF && data[i+1] == 0xD9 {
			return data[:i+2], true
		}
	}
	if len(data) > 100 {
		return data, true // truncated but plausible
	}
	return nil, false
}

// tryPNG extracts a PNG (89 50 4E 47 ... IEND) from the start of data.
func tryPNG(data []byte) ([]byte, bool) {
	sig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}
	if len(data) < 8 {
		return nil, false
	}
	for i, b := range sig {
		if data[i] != b {
			return nil, false
		}
	}
	iend := []byte{0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82}
	for i := 8; i+12 <= len(data); i++ {
		match := true
		for j, b := range iend {
			if data[i+4+j] != b {
				match = false
				break
			}
		}
		if match {
			return data[:i+12], true
		}
	}
	if len(data) > 100 {
		return data, true
	}
	return nil, false
}

func test_doc_extractor() {
	docPath := "test.doc"
	outDir := "."
	if len(os.Args) > 1 {
		docPath = os.Args[1]
	}
	if len(os.Args) > 2 {
		outDir = os.Args[2]
	}

	fmt.Fprintf(os.Stderr, "Opening: %s\n", docPath)

	cfb, err := openCFB(docPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening CFB: %v\n", err)
		os.Exit(1)
	}
	defer cfb.f.Close()

	// ── CFB directory listing ────────────────────────────────────────────
	fmt.Fprintln(os.Stderr, "\n── CFB directory entries ──")
	for i, de := range cfb.dirs {
		if de.ObjType != 0 {
			typeStr := "storage"
			switch de.ObjType {
			case 2:
				typeStr = "stream "
			case 5:
				typeStr = "root   "
			}
			fmt.Fprintf(os.Stderr, "  [%02d] %s  %q  (size=%d)\n",
				i, typeStr, dirName(de), de.SizeLow)
		}
	}

	// ── Text ─────────────────────────────────────────────────────────────
	fmt.Fprintln(os.Stderr, "\n── Extracting text ──")
	text, err := extractText(cfb)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Text extraction error: %v\n", err)
	}
	if text == "" {
		fmt.Fprintln(os.Stderr, "WARNING: No text extracted.")
	}
	fmt.Println(text)
	fmt.Fprintf(os.Stderr, "Text: %d chars, ~%d lines\n",
		len([]rune(text)), strings.Count(text, "\n")+1)

	// ── Images ───────────────────────────────────────────────────────────
	fmt.Fprintln(os.Stderr, "\n── Extracting images ──")
	images, err := extractImages(cfb)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Image extraction error: %v\n", err)
	}

	if len(images) == 0 {
		fmt.Fprintln(os.Stderr, "No images found.")
	} else {
		fmt.Fprintf(os.Stderr, "Found %d image(s):\n", len(images))
		base := strings.TrimSuffix(filepath.Base(docPath), filepath.Ext(docPath))
		for _, img := range images {
			fname := filepath.Join(outDir,
				fmt.Sprintf("%s_image_%03d.%s", base, img.index, img.ext))
			if werr := os.WriteFile(fname, img.data, 0644); werr != nil {
				fmt.Fprintf(os.Stderr, "  [%03d] ERROR writing %s: %v\n",
					img.index, fname, werr)
			} else {
				fmt.Fprintf(os.Stderr, "  [%03d] Saved %s (%d bytes)\n",
					img.index, fname, len(img.data))
			}
		}
	}

	fmt.Fprintln(os.Stderr, "\n── Done ──")
}
