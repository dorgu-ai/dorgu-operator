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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Clean(tt.in, tt.keepNewlines))
		})
	}
}
