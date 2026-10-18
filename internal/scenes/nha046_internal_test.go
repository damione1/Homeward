package scenes

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA046RetainedBudgetCoversBackingArrays(t *testing.T) {
	id, err := domain.ParseHAAlias("scene.nha046_budget")
	if err != nil {
		t.Fatal(err)
	}
	memberAlias, err := domain.ParseHAAlias("switch." + strings.Repeat("x", 200))
	if err != nil {
		t.Fatal(err)
	}
	entry := &temporaryEntry{
		members:   make([]memberSpec, 2, 8),
		omissions: make([]MemberResult, 1, 4),
		epoch:     "synthetic-epoch",
	}
	entry.members[0].alias = memberAlias
	entry.members[1].alias = memberAlias
	entry.members[1].omissionReason = "fresh observed value is absent"
	entry.omissions[0] = MemberResult{alias: memberAlias, reason: "fresh observed value is absent"}
	minimum := int(unsafe.Sizeof(*entry)) + cap(entry.members)*int(unsafe.Sizeof(memberSpec{})) +
		cap(entry.omissions)*int(unsafe.Sizeof(MemberResult{})) + len(entry.epoch)
	if charge := temporaryBytes(id, entry); charge < minimum {
		t.Fatalf("retained charge %d is smaller than backing arrays and entry %d", charge, minimum)
	}
}
