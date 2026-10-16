package app

import "strings"

// ThinkStrip preserves partial marker prefixes between worker chunks.
type ThinkStrip struct {
	buffer string
	hidden bool
	Allow  bool
}

func (s *ThinkStrip) Feed(text string, final bool) string {
	if s.Allow {
		return text
	}
	s.buffer += text
	var out strings.Builder
	for s.buffer != "" {
		marker := "<think>"
		if s.hidden {
			marker = "</think>"
		}
		if i := strings.Index(s.buffer, marker); i >= 0 {
			if !s.hidden {
				out.WriteString(s.buffer[:i])
			}
			s.buffer = s.buffer[i+len(marker):]
			s.hidden = !s.hidden
			continue
		}
		keep := 0
		if !final {
			for n := 1; n < len(marker) && n <= len(s.buffer); n++ {
				if strings.HasSuffix(s.buffer, marker[:n]) {
					keep = n
				}
			}
		}
		if !s.hidden {
			out.WriteString(s.buffer[:len(s.buffer)-keep])
		}
		s.buffer = s.buffer[len(s.buffer)-keep:]
		break
	}
	return out.String()
}
