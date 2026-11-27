package observability

import "strings"

// MaskEmail masks an email address for logs and timelines:
// "alice@example.com" → "a***@e***.com". Input that is not a plausible email is
// fully masked rather than echoed.
func MaskEmail(s string) string {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return "***"
	}
	local, domain := s[:at], s[at+1:]
	dot := strings.LastIndexByte(domain, '.')
	if dot <= 0 || dot == len(domain)-1 {
		return firstRune(local) + "***@***"
	}
	return firstRune(local) + "***@" + firstRune(domain[:dot]) + "***" + domain[dot:]
}

// MaskPhone keeps the leading "+CC" (up to 2 digits after "+") and the last 4
// digits: "+919999991234" → "+91******1234". Non-digits are ignored. Numbers
// too short to mask safely are fully masked.
func MaskPhone(s string) string {
	plus := strings.HasPrefix(strings.TrimSpace(s), "+")
	var digits []byte
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			digits = append(digits, s[i])
		}
	}
	const keepTail = 4
	ccLen := 0
	if plus {
		ccLen = 2
	}
	if len(digits) < ccLen+keepTail+2 {
		return "***"
	}
	var b strings.Builder
	if plus {
		b.WriteByte('+')
		b.Write(digits[:ccLen])
	}
	b.WriteString(strings.Repeat("*", len(digits)-ccLen-keepTail))
	b.Write(digits[len(digits)-keepTail:])
	return b.String()
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}
