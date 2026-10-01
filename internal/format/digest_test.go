// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// TestExamDigestRendering pins the digest's output in every renderer.
func TestExamDigestRendering(t *testing.T) {
	t.Parallel()

	desc := []string{"pon 5.10.", "pet 9.10."}
	fields := []string{"Matematika – pisana provjera", "Fizika"}

	tests := []struct {
		name   string
		render func(string, string, msgtypes.EventCode, []string, []string) string
		want   string
	}{
		{
			name:   "plain",
			render: PlainMsg,
			want: ExamDigestPrefix + "pero.peric / 5.10. – 11.10.\n\n" +
				"pon 5.10.: Matematika – pisana provjera\npet 9.10.: Fizika\n",
		},
		{
			name:   "html",
			render: HTMLMsg,
			want: "<b>" + ExamDigestPrefix + "pero.peric / 5.10. – 11.10.</b>\n<pre>\n" +
				"pon 5.10.: Matematika – pisana provjera\npet 9.10.: Fizika\n</pre>\n",
		},
		{
			name:   "markup",
			render: MarkupMsg,
			want: "*" + ExamDigestPrefix + "pero.peric / 5.10. – 11.10.*\n\n```\n" +
				"pon 5.10.: Matematika – pisana provjera\npet 9.10.: Fizika\n```\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.render("pero.peric", "5.10. – 11.10.", msgtypes.ExamDigest, desc, fields); got != tt.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}
