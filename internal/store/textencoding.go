package store

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/saintfish/chardet"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

const defaultTextEncoding = "utf-8"

type textCodec struct {
	codec encoding.Encoding
	bom   []byte
	utf32 bool
	big   bool
}

var textCodecs = map[string]textCodec{
	"utf-8":        {},
	"utf-8bom":     {bom: []byte{0xef, 0xbb, 0xbf}},
	"utf-16le":     {codec: unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)},
	"utf-16le-bom": {codec: unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), bom: []byte{0xff, 0xfe}},
	"utf-16be":     {codec: unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), big: true},
	"utf-16be-bom": {codec: unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), bom: []byte{0xfe, 0xff}, big: true},
	"utf-32le":     {utf32: true},
	"utf-32le-bom": {bom: []byte{0xff, 0xfe, 0x00, 0x00}, utf32: true},
	"utf-32be":     {utf32: true, big: true},
	"utf-32be-bom": {bom: []byte{0x00, 0x00, 0xfe, 0xff}, utf32: true, big: true},
	"gb18030":      {codec: simplifiedchinese.GB18030},
	"gbk":          {codec: simplifiedchinese.GBK},
	"big5":         {codec: traditionalchinese.Big5},
	"shift_jis":    {codec: japanese.ShiftJIS},
	"euc-jp":       {codec: japanese.EUCJP},
	"iso-2022-jp":  {codec: japanese.ISO2022JP},
	"euc-kr":       {codec: korean.EUCKR},
	"windows-874":  {codec: charmap.Windows874},
	"windows-1250": {codec: charmap.Windows1250},
	"windows-1251": {codec: charmap.Windows1251},
	"windows-1252": {codec: charmap.Windows1252},
	"windows-1253": {codec: charmap.Windows1253},
	"windows-1254": {codec: charmap.Windows1254},
	"windows-1255": {codec: charmap.Windows1255},
	"windows-1256": {codec: charmap.Windows1256},
	"windows-1257": {codec: charmap.Windows1257},
	"windows-1258": {codec: charmap.Windows1258},
	"iso-8859-2":   {codec: charmap.ISO8859_2},
	"iso-8859-3":   {codec: charmap.ISO8859_3},
	"iso-8859-4":   {codec: charmap.ISO8859_4},
	"iso-8859-5":   {codec: charmap.ISO8859_5},
	"iso-8859-6":   {codec: charmap.ISO8859_6},
	"iso-8859-7":   {codec: charmap.ISO8859_7},
	"iso-8859-8":   {codec: charmap.ISO8859_8},
	"iso-8859-9":   {codec: charmap.ISO8859_9},
	"iso-8859-10":  {codec: charmap.ISO8859_10},
	"iso-8859-13":  {codec: charmap.ISO8859_13},
	"iso-8859-14":  {codec: charmap.ISO8859_14},
	"iso-8859-15":  {codec: charmap.ISO8859_15},
	"koi8-r":       {codec: charmap.KOI8R},
	"koi8-u":       {codec: charmap.KOI8U},
	"ibm866":       {codec: charmap.CodePage866},
	"macintosh":    {codec: charmap.Macintosh},
	"mac-cyrillic": {codec: charmap.MacintoshCyrillic},
}

var encodingAliases = map[string]string{
	"ascii": "utf-8", "us-ascii": "utf-8", "utf8": "utf-8",
	"utf-8-sig": "utf-8bom", "utf8bom": "utf-8bom",
	"utf-16": "utf-16le-bom", "utf16le": "utf-16le", "utf16be": "utf-16be",
	"utf-32": "utf-32le-bom", "utf32le": "utf-32le", "utf32be": "utf-32be",
	"gb2312": "gbk", "gb-2312": "gbk", "gb-18030": "gb18030", "x-gbk": "gbk",
	"shift-jis": "shift_jis", "sjis": "shift_jis", "ms932": "shift_jis", "windows-31j": "shift_jis",
	"iso8859-1": "windows-1252", "iso-8859-1": "windows-1252", "latin1": "windows-1252",
	"tis-620": "windows-874", "iso-8859-11": "windows-874",
	"iso-8859-8-i": "iso-8859-8", "mac-cyrillic": "mac-cyrillic", "x-mac-cyrillic": "mac-cyrillic",
}

func normalizeTextEncoding(value string) (string, bool) {
	name := strings.ToLower(strings.TrimSpace(value))
	name = strings.ReplaceAll(name, "_", "-")
	if alias, ok := encodingAliases[name]; ok {
		name = alias
	}
	if name == "shift-jis" {
		name = "shift_jis"
	}
	_, ok := textCodecs[name]
	return name, ok
}

func bomEncoding(content []byte) string {
	for _, candidate := range []struct {
		bom  []byte
		name string
	}{
		{[]byte{0x00, 0x00, 0xfe, 0xff}, "utf-32be-bom"},
		{[]byte{0xff, 0xfe, 0x00, 0x00}, "utf-32le-bom"},
		{[]byte{0xef, 0xbb, 0xbf}, "utf-8bom"},
		{[]byte{0xfe, 0xff}, "utf-16be-bom"},
		{[]byte{0xff, 0xfe}, "utf-16le-bom"},
	} {
		if bytes.HasPrefix(content, candidate.bom) {
			return candidate.name
		}
	}
	return ""
}

func unicodeEncodingWithoutBOM(content []byte) string {
	// UTF-32 text has at least one zero byte in the high half of nearly every
	// scalar value. Check it before UTF-16 because the same byte stream also has
	// a strong alternating-zero pattern when viewed as 16-bit units.
	if len(content) >= 8 && len(content)%4 == 0 {
		units := len(content) / 4
		for _, candidate := range []struct {
			name      string
			positions [2]int
		}{
			{"utf-32le", [2]int{2, 3}},
			{"utf-32be", [2]int{0, 1}},
		} {
			zeros := 0
			for offset := 0; offset < len(content); offset += 4 {
				for _, position := range candidate.positions {
					if content[offset+position] == 0 {
						zeros++
					}
				}
			}
			if zeros >= units {
				if decoded, err := decodeWithEncoding(content, candidate.name); err == nil && likelyText(decoded) {
					return candidate.name
				}
			}
		}
	}
	if len(content) >= 8 && len(content)%2 == 0 {
		units := len(content) / 2
		for _, candidate := range []struct {
			name         string
			highPosition int
		}{
			{"utf-16le", 1},
			{"utf-16be", 0},
		} {
			highZeros, lowZeros := 0, 0
			for offset := 0; offset < len(content); offset += 2 {
				high := offset + candidate.highPosition
				low := offset + 1 - candidate.highPosition
				if content[high] == 0 {
					highZeros++
				}
				if content[low] == 0 {
					lowZeros++
				}
			}
			if highZeros*100 >= units*30 && highZeros >= lowZeros*2 {
				if decoded, err := decodeWithEncoding(content, candidate.name); err == nil && likelyText(decoded) {
					return candidate.name
				}
			}
		}
	}
	return ""
}

func decodeWithEncoding(content []byte, name string) (string, error) {
	canonical, ok := normalizeTextEncoding(name)
	if !ok {
		return "", fmt.Errorf("unsupported text encoding %q", name)
	}
	codec := textCodecs[canonical]
	data := content
	if len(codec.bom) > 0 && bytes.HasPrefix(data, codec.bom) {
		data = data[len(codec.bom):]
	}
	if codec.utf32 {
		return decodeUTF32(data, codec.big)
	}
	if codec.codec == nil {
		if !utf8.Valid(data) {
			return "", errors.New("invalid UTF-8")
		}
		return string(data), nil
	}
	decoded, _, err := transform.Bytes(codec.codec.NewDecoder(), data)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(decoded) {
		return "", errors.New("decoder produced invalid UTF-8")
	}
	// Several x/text decoders recover from malformed byte sequences by
	// inserting U+FFFD. A web editor must never silently show and later save
	// that replacement as real content. Re-encoding only when U+FFFD appears
	// distinguishes a literal replacement character (notably in UTF-16) from
	// one synthesized by a lossy decoder.
	if bytes.Contains(decoded, []byte(string(utf8.RuneError))) {
		reencoded, _, encodeErr := transform.Bytes(codec.codec.NewEncoder(), decoded)
		if encodeErr != nil || !bytes.Equal(reencoded, data) {
			return "", errors.New("text contains an invalid byte sequence")
		}
	}
	return string(decoded), nil
}

func encodeWithEncoding(content, name string) ([]byte, string, error) {
	canonical := defaultTextEncoding
	if strings.TrimSpace(name) != "" {
		var ok bool
		canonical, ok = normalizeTextEncoding(name)
		if !ok {
			return nil, "", refusalf("不支持的文本编码 %q", name)
		}
	}
	if !utf8.ValidString(content) {
		return nil, "", refusal("编辑器内容不是有效的 Unicode 文本")
	}
	codec := textCodecs[canonical]
	var encoded []byte
	var err error
	if codec.utf32 {
		encoded = encodeUTF32(content, codec.big)
	} else if codec.codec == nil {
		encoded = []byte(content)
	} else {
		encoded, _, err = transform.Bytes(codec.codec.NewEncoder(), []byte(content))
		if err != nil {
			return nil, "", refusalf("内容含有 %s 无法表示的字符: %v", canonical, err)
		}
	}
	if len(codec.bom) > 0 {
		encoded = append(bytes.Clone(codec.bom), encoded...)
	}
	return encoded, canonical, nil
}

func detectAndDecodeText(content []byte, hint string) (string, string, error) {
	if strings.TrimSpace(hint) != "" {
		if canonical, ok := normalizeTextEncoding(hint); ok {
			if decoded, err := decodeWithEncoding(content, canonical); err == nil && likelyText(decoded) {
				return decoded, canonical, nil
			}
		}
	}
	if detected := bomEncoding(content); detected != "" {
		decoded, err := decodeWithEncoding(content, detected)
		if err == nil && likelyText(decoded) {
			return decoded, detected, nil
		}
	}
	if utf8.Valid(content) {
		decoded := string(content)
		if likelyText(decoded) {
			return decoded, defaultTextEncoding, nil
		}
	}
	if detected := unicodeEncodingWithoutBOM(content); detected != "" {
		decoded, err := decodeWithEncoding(content, detected)
		if err == nil {
			return decoded, detected, nil
		}
	}
	result, err := chardet.NewTextDetector().DetectBest(content)
	if err != nil || result.Confidence < 20 {
		return "", "", errors.New("text encoding could not be detected")
	}
	canonical, ok := normalizeTextEncoding(result.Charset)
	if !ok {
		return "", "", fmt.Errorf("detected unsupported text encoding %q", result.Charset)
	}
	decoded, err := decodeWithEncoding(content, canonical)
	if err != nil || !likelyText(decoded) {
		return "", "", errors.New("content does not look like text")
	}
	return decoded, canonical, nil
}

func likelyText(content string) bool {
	if content == "" {
		return true
	}
	controls, total := 0, 0
	for _, r := range content {
		total++
		if r == 0 {
			return false
		}
		if (r < 0x20 && r != '\t' && r != '\n' && r != '\r' && r != '\f') || (r >= 0x7f && r <= 0x9f) {
			controls++
		}
	}
	return controls*100 <= total*2
}

func decodeUTF32(content []byte, bigEndian bool) (string, error) {
	if len(content)%4 != 0 {
		return "", errors.New("invalid UTF-32 byte length")
	}
	result := make([]rune, 0, len(content)/4)
	for i := 0; i < len(content); i += 4 {
		var value uint32
		if bigEndian {
			value = uint32(content[i])<<24 | uint32(content[i+1])<<16 | uint32(content[i+2])<<8 | uint32(content[i+3])
		} else {
			value = uint32(content[i]) | uint32(content[i+1])<<8 | uint32(content[i+2])<<16 | uint32(content[i+3])<<24
		}
		if value > utf8.MaxRune || value >= 0xd800 && value <= 0xdfff {
			return "", errors.New("invalid UTF-32 code point")
		}
		result = append(result, rune(value))
	}
	return string(result), nil
}

func encodeUTF32(content string, bigEndian bool) []byte {
	encoded := make([]byte, 0, len([]rune(content))*4)
	for _, r := range content {
		value := uint32(r)
		if bigEndian {
			encoded = append(encoded, byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
		} else {
			encoded = append(encoded, byte(value), byte(value>>8), byte(value>>16), byte(value>>24))
		}
	}
	return encoded
}

// EncodeText converts the Unicode text posted by the browser back to the
// resource's chosen file encoding.
func EncodeText(content, encodingName string) ([]byte, string, error) {
	return encodeWithEncoding(content, encodingName)
}

// DecodeText turns stored bytes into the Unicode string used by the editor.
// The hint comes from persisted resource metadata; detection is the fallback
// for files created before that metadata existed.
func DecodeText(content []byte, hint string) (string, string, error) {
	return detectAndDecodeText(content, hint)
}

// Line endings are a property of the file, not of its text - the same rule
// every editor follows. Detection happens on read, the value rides the form
// like the encoding does, and the content is written back with it. A file
// therefore keeps the endings it arrived with, and a file whose endings are
// mixed is unified on the first save rather than kept mixed forever.
const (
	EOLLF   = "lf"
	EOLCRLF = "crlf"
)

// NormalizeEOLName maps whatever arrived in a form to one of the two endings
// that are actually written. Anything unrecognised is LF.
func NormalizeEOLName(name string) string {
	if strings.EqualFold(strings.TrimSpace(name), EOLCRLF) {
		return EOLCRLF
	}
	return EOLLF
}

// DetectEOL picks the ending a text is written with, by majority. Only CRLF
// and LF are produced: a lone CR is a line break for counting purposes but is
// never chosen as the file's ending, which is what VS Code, Sublime and the
// JetBrains editors all settle on. Text with no line break at all is LF.
func DetectEOL(text string) string {
	crlf := strings.Count(text, "\r\n")
	// Every LF that is not the tail of a CRLF, plus every CR that is not its head.
	lf := strings.Count(text, "\n") - crlf
	cr := strings.Count(text, "\r") - crlf
	if crlf > lf+cr {
		return EOLCRLF
	}
	return EOLLF
}

// ApplyEOL rewrites every line break in text to the given ending. It is the
// last step before encoding, so that a browser that normalised the textarea to
// CRLF on submit - which every browser does, with or without JavaScript - does
// not thereby convert an LF file.
func ApplyEOL(text, eol string) string {
	unified := strings.ReplaceAll(text, "\r\n", "\n")
	unified = strings.ReplaceAll(unified, "\r", "\n")
	if NormalizeEOLName(eol) == EOLCRLF {
		return strings.ReplaceAll(unified, "\n", "\r\n")
	}
	return unified
}
