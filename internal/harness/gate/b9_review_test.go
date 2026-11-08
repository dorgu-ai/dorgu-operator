package gate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// R-13. The "survive truncation" assertion in TestRecurrenceName never truncates:
// 246 + len("-r12") = 250 <= 253. This test uses a base that really needs shortening
// (252 chars is the longest generateIncidentName can emit) and shows the lineage
// of generation 1 no longer equals the lineage of generation 0, so every
// cooldown that compares lineages would let the recurrence through.
//
// UNREACHABLE TODAY: ns <= 63, persona segment <= 78, signal names <= ~30 gives
// bases <= ~190. It becomes live if a segment cap or signal name grows. Expected
// to FAIL; it is a latent defect and a vacuous test, not a present exploit.
func TestReview_LineageSurvivesTruncationOfALongBase_Latent(t *testing.T) {
	base := "im-" + strings.Repeat("a", 236) + "-" + strings.Repeat("b", 12) // 252
	assert.True(t, SameIncidentLineage(base, RecurrenceName(base, 1)),
		"generation 1 of a 252-char base must stay in the lineage of generation 0")
}
