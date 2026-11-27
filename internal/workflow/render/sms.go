package render

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf16"
)

// SMSEncoding is how an SMS is encoded on the wire.
type SMSEncoding string

// SMS encodings (3GPP TS 23.038).
const (
	GSM7 SMSEncoding = "gsm7"
	UCS2 SMSEncoding = "ucs2"
)

// SMSInfo describes an SMS body's encoding and segmentation.
type SMSInfo struct {
	Encoding SMSEncoding
	Segments int
	Units    int      // septets (GSM-7) or UTF-16 code units (UCS-2)
	UCS2     []string // characters that forced UCS-2, deduplicated, in order
}

// SMSOutput is a rendered SMS.
type SMSOutput struct {
	Text string
	SMSInfo
}

// gsmBasic is the GSM 03.38 default alphabet (one septet each).
const gsmBasic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"

// gsmExt is the GSM 03.38 extension table (escape + char: two septets each).
const gsmExt = "^{}\\[~]|€\f"

var gsmWidth = func() map[rune]int {
	m := make(map[rune]int, 140)
	for _, r := range gsmBasic {
		m[r] = 1
	}
	for _, r := range gsmExt {
		m[r] = 2
	}
	return m
}()

// SMSSegments computes encoding and segment count. Multipart segments never
// split an escape sequence or a surrogate pair, matching what carriers do.
func SMSSegments(s string) SMSInfo {
	info := SMSInfo{Encoding: GSM7}
	widths := make([]int, 0, len(s))
	seen := map[rune]bool{}
	for _, r := range s {
		w, ok := gsmWidth[r]
		if !ok {
			info.Encoding = UCS2
			if !seen[r] && len(info.UCS2) < 20 {
				seen[r] = true
				info.UCS2 = append(info.UCS2, string(r))
			}
		}
		widths = append(widths, w)
	}
	single, multi := 160, 153
	if info.Encoding == UCS2 {
		single, multi = 70, 67
		widths = widths[:0]
		for _, r := range s {
			widths = append(widths, len(utf16.Encode([]rune{r})))
		}
	} else {
		info.UCS2 = nil
	}
	for _, w := range widths {
		info.Units += w
	}
	switch {
	case info.Units == 0:
		info.Segments = 0
	case info.Units <= single:
		info.Segments = 1
	default:
		seg, used := 1, 0
		for _, w := range widths {
			if used+w > multi {
				seg++
				used = 0
			}
			used += w
		}
		info.Segments = seg
	}
	return info
}

// ValidateSMS checks syntax (save time).
func ValidateSMS(text string) error {
	if strings.TrimSpace(text) == "" {
		return Errors{{Field: "text", Msg: "text is required"}}
	}
	if _, err := field("text", text); err != nil {
		return Errors{asError(err)}
	}
	return nil
}

// SMS renders an SMS and enforces the 3-segment cap.
func SMS(ctx context.Context, text string, in Input) (SMSOutput, error) {
	if err := ValidateSMS(text); err != nil {
		return SMSOutput{}, err
	}
	t, _ := field("text", text)
	// 3 UCS-2 segments of 67 units is the most that can pass, and a UTF-8
	// rune is at most 4 bytes, so 3*160*4 bounds any valid output.
	out, err := exec("text", t, ctxOf(ctx), in.data(), EscapeNone, MaxSMSSegments*160*4)
	if err != nil {
		return SMSOutput{}, err
	}
	out = strings.TrimSpace(out)
	info := SMSSegments(out)
	if info.Segments == 0 {
		return SMSOutput{}, &Error{Field: "text", Msg: "SMS rendered empty"}
	}
	if info.Segments > MaxSMSSegments {
		return SMSOutput{}, &Error{Field: "text", Msg: fmt.Sprintf("SMS is %d segments (%s, %d units); the limit is %d", info.Segments, info.Encoding, info.Units, MaxSMSSegments)}
	}
	return SMSOutput{Text: out, SMSInfo: info}, nil
}
