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

package safetext

import (
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
)

func r(codes ...rune) string { return string(codes) }

func TestClean(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		keepNewlines bool
		want         string
	}{
		{"plain text is untouched", "raise the memory limit to 384Mi", false, "raise the memory limit to 384Mi"},
		{"non-ASCII text is untouched", r('m', 0xE9, 'm', 0x5185, 0x5B58), false, r('m', 0xE9, 'm', 0x5185, 0x5B58)},
		{"ANSI escape is stripped, the words stay", "\x1b[2J\x1b[31mAPPROVE ME\x1b[0m", false, "[2J[31mAPPROVE ME[0m"},
		{"C0 controls", "a\x00b\x07c\x08d\re", false, "abcde"},
		{"DEL", "a\x7fb", false, "ab"},
		{"C1 controls", r('a', 0x85, 'b', 0x9B, 'c'), false, "abc"},
		{"line and paragraph separators", r('a', 0x2028, 'b', 0x2029, 'c'), true, "abc"},
		{"bidi controls", r('a', 0x202E, 'b', 0x2066, 'c', 0x2069, 'd', 0x200F, 'e'), false, "abcde"},
		{"zero width and BOM", r('a', 0x200B, 'b', 0x200D, 'c', 0xFEFF, 'd'), false, "abcd"},
		{"newline and tab kept when multi-line", "a\nb\tc", true, "a\nb\tc"},
		{"newline and tab removed when not", "a\nb\tc", false, "abc"},
		{"carriage return never survives", "a\r\nb", true, "a\nb"},
		{"invalid UTF-8 is replaced", "a\xffb", false, r('a', 0xFFFD, 'b')},
		{"empty", "", true, ""},
		{"Unicode tag characters (invisible ASCII)", r('a', 0xE0041, 0xE0042, 'b', 0xE0000, 0xE007F, 'c'), false, "abc"},
		{"word joiner and invisible operators", r('a', 0x2060, 'b', 0x2061, 0x2064, 'c', 0x2065, 0x206F, 'd'), false, "abcd"},
		{"Arabic letter mark", r('a', 0x061C, 'b'), false, "ab"},
		{"soft hyphen", r('a', 0xAD, 'b'), false, "ab"},
		{"variation selectors", r('a', 0xFE00, 'b', 0xFE0F, 'c', 0xE0100, 'd', 0xE01EF, 'e'), false, "abcde"},
		{"interlinear annotation", r('a', 0xFFF9, 'b', 0xFFFB, 'c'), false, "abc"},
		{"left to right isolate and Mongolian vowel separator", r('a', 0x180E, 'b', 0x2066, 'c'), false, "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Clean(tt.in, tt.keepNewlines))
		})
	}
}

// Every character of the format category is removed, so the set cannot fall behind
// Unicode: this walks the whole code space instead of trusting a list.
func TestClean_RemovesEveryFormatCharacter(t *testing.T) {
	for c := rune(0); c <= unicode.MaxRune; c++ {
		if !unicode.Is(unicode.Cf, c) || c == unicode.ReplacementChar {
			continue
		}
		if got := Clean("a"+string(c)+"b", false); got != "ab" {
			t.Fatalf("U+%04X (category Cf) survived Clean: %q", c, got)
		}
	}
}

// The step-command rule of the CRD (api/v1.SanitizeStepCommand) refuses control
// characters, every Cf and the two separators. Clean removes at least that set.
func TestClean_CoversTheStepCommandCharacterClass(t *testing.T) {
	for c := rune(0); c <= unicode.MaxRune; c++ {
		if !unicode.IsControl(c) && !unicode.Is(unicode.Cf, c) && c != 0x2028 && c != 0x2029 {
			continue
		}
		if got := Clean("a"+string(c)+"b", false); got != "ab" {
			t.Fatalf("U+%04X is refused in a step command but survived Clean: %q", c, got)
		}
	}
}
