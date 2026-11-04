package tasknames

import (
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/lists"
	"strings"
	"testing"
)

func TestNHA202NamesPreserveImportedIdentityAndRejectAlternateSpellings(t *testing.T) {
	home, _ := apitypes.ParseHomeName("homes/synthetic-home")
	for _, raw := range []string{"plain-id", " Imported / tâche 同じ ", strings.Repeat("x", 255)} {
		id, err := domain.ParseListID(raw)
		if err != nil {
			t.Fatal(err)
		}
		uid, err := lists.ParseItemUID(raw)
		if err != nil {
			t.Fatal(err)
		}
		list, err := List(home, id)
		if err != nil {
			t.Fatal(err)
		}
		task, err := Task(list, uid)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := ParseTask(task.String())
		if err != nil || decoded != task {
			t.Fatalf("lossless resource roundtrip: %v", err)
		}
		for _, bad := range []string{task.String() + "/", strings.ToUpper(task.String()), "/" + task.String(), strings.Replace(task.String(), "/tasks/", "/task/", 1)} {
			if _, err := ParseTask(bad); err == nil {
				t.Fatal("admitted alternate resource spelling")
			}
		}
	}
}
func TestNHA202CompoundEtagBindsIdentityAndBothRevisions(t *testing.T) {
	first := "homes/synthetic/taskLists/id-61/tasks/id-62"
	other := "homes/synthetic/taskLists/id-61/tasks/id-63"
	etag, err := Etag(first, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	otherETag, _ := Etag(other, 7, 3)
	if otherETag == etag {
		t.Fatal("resource identity omitted from etag")
	}
	parentETag, _ := Etag(first, 7, 0)
	if parentETag == etag {
		t.Fatal("item revision omitted from parent etag")
	}
	changed, _ := Etag(first, 8, 3)
	if changed == etag {
		t.Fatal("collection ordering revision omitted from etag")
	}
	changed, _ = Etag(first, 7, 4)
	if changed == etag {
		t.Fatal("item revision omitted from etag")
	}
}
