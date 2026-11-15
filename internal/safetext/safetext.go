/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package safetext cleans untrusted text before the operator stores or shows it.
//
// A proposal's rationale and step text come from outside (an agent, a script, an
// attacker). They end up in a person's terminal and in the dashboard, where a
// terminal escape sequence can clear the screen or recolour a line, and a bidi
// control can make "approve" read as something else. Clean removes the characters
// that do that and keeps the words.
package safetext

import (
	"strings"
	"unicode"
)

// Clean returns s with every control or invisible character removed: C0
// (U+0000 to U+001F), DEL, C1 (U+0080 to U+009F), the line and paragraph
// separators U+2028 and U+2029, and every format character (Unicode category Cf).
// The format characters include the bidirectional controls (U+061C, U+200E,
// U+200F, U+202A to U+202E, U+2066 to U+2069), the zero-width characters (U+200B
// to U+200D), the word joiner and invisible operators (U+2060 to U+206F), the
// soft hyphen (U+00AD), the byte order mark (U+FEFF) and the Unicode tag
// characters (U+E0000 to U+E007F), which spell ASCII that no one can see. The
// variation selectors (U+FE00 to U+FE0F, U+E0100 to U+E01EF) go too.
//
// That is a superset of the class the step-command rule refuses (api/v1
// SanitizeStepCommand: control, Cf, U+2028, U+2029), and a test holds the two
// together. Invalid UTF-8 is replaced. Newline and tab survive when keepNewlines
// is set, for a field that is multi-line by design.
func Clean(s string, keepNewlines bool) string {
	s = strings.ToValidUTF8(s, string(unicode.ReplacementChar))
	return strings.Map(func(r rune) rune {
		switch {
		case keepNewlines && (r == '\n' || r == '\t'):
			return r
		case invisible(r):
			return -1
		}
		return r
	}, s)
}

func invisible(r rune) bool {
	switch {
	case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == 0x2028, r == 0x2029:
		return true
	case r >= 0x2060 && r <= 0x206F: // includes U+2065, which is unassigned
		return true
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF: // variation selectors
		return true
	case r >= 0xE0000 && r <= 0xE007F: // tag characters, including the unassigned
		return true
	}
	return false
}
