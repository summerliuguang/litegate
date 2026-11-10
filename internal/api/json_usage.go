package api

import "encoding/json"

// jsonUsageScanner observes a top-level JSON usage object without retaining response
// content. It handles escaped strings and chunk boundaries. Only the usage object
// (at most 64 KiB) and a bounded field name are retained; huge content is skipped.
// The transport remains byte-for-byte passthrough even for malformed JSON.
type jsonUsageScanner struct {
	message, messageKey, topFound           bool
	captureDepth                            int
	depth                                   int
	inString, escaped, keyString, expectKey bool
	key                                     []byte
	usageKey, afterColon                    bool
	capture, overflow                       bool
	buf                                     []byte
	usage                                   tokenUsage
}

func (s *jsonUsageScanner) Write(p []byte) (int, error) {
	for _, c := range p {
		if s.capture {
			if len(s.buf) < maxUsageTail {
				s.buf = append(s.buf, c)
			} else {
				s.overflow = true
			}
		}
		if s.inString {
			if s.keyString && len(s.key) < 128 {
				s.key = append(s.key, c)
			}
			if s.escaped {
				s.escaped = false
				continue
			}
			if c == '\\' {
				s.escaped = true
				continue
			}
			if c == '"' {
				s.inString = false
				if s.keyString {
					var key string
					valid := json.Unmarshal(s.key, &key) == nil
					s.usageKey = valid && key == "usage"
					s.messageKey = valid && key == "message" && s.depth == 1
					s.keyString = false
					s.expectKey = false
				}
			}
			continue
		}
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		if (s.depth == 1 || (s.depth == 2 && s.message)) && s.afterColon {
			s.afterColon = false
			if s.usageKey && c == '{' {
				s.capture = true
				s.captureDepth = s.depth
				s.overflow = false
				s.buf = append(s.buf[:0], c)
			}
			s.usageKey = false
			if s.messageKey && c == '{' {
				s.message = true
				s.expectKey = true
			}
			s.messageKey = false
		}
		switch c {
		case '"':
			s.inString = true
			s.keyString = (s.depth == 1 || (s.depth == 2 && s.message)) && s.expectKey
			if s.keyString {
				s.key = append(s.key[:0], c)
			}
		case '{', '[':
			s.depth++
			if s.depth == 1 && c == '{' {
				s.expectKey = true
			}
		case '}', ']':
			s.depth--
			if s.capture && s.depth == s.captureDepth {
				s.capture = false
				var fields usageFields
				if !s.overflow && json.Unmarshal(s.buf, &fields) == nil {
					if s.captureDepth == 1 || !s.topFound {
						s.usage = fields.values()
					}
					if s.captureDepth == 1 {
						s.topFound = true
					}
				}
			}
			if s.depth < 2 {
				s.message = false
			}
		case ':':
			if s.depth == 1 || (s.depth == 2 && s.message) {
				s.afterColon = true
			}
		case ',':
			if s.depth == 1 || (s.depth == 2 && s.message) {
				s.expectKey = true
				s.usageKey = false
			}
		}
	}
	return len(p), nil
}
