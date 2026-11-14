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

// Clean returns s with every control character removed: C0 (U+0000 to U+001F), DEL,
// C1 (U+0080 to U+009F), the line and paragraph separators U+2028 and U+2029, the
// bidirectional controls (U+200E, U+200F, U+202A to U+202E, U+2066 to U+2069), the
// zero-width characters (U+200B to U+200D) and the byte order mark (U+FEFF).
// Invalid UTF-8 is replaced. Newline and tab survive when keepNewlines is set, for
// a field that is multi-line by design.
func Clean(s string, keepNewlines bool) string {
	s = strings.ToValidUTF8(s, string(unicode.ReplacementChar))
	return strings.Map(func(r rune) rune {
		switch {
		case keepNewlines && (r == '\n' || r == '\t'):
			return r
		case unicode.IsControl(r), r == 0x2028, r == 0x2029, r == 0xFEFF,
			r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, s)
}
