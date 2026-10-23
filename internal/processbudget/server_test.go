package processbudget

import (
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

// Separate real processes exercise Go's startup GOMEMLIMIT parsing and keep
// this test from changing the budget of an unrelated test runner/embedding.
func TestNHA165ServerMemoryBudgetPreservesOperatorStartupLimit(t *testing.T) {
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	for _, c := range []struct {
		name, value string
		want        int64
	}{{"default", "", ServerGoMemoryLimit}, {"stricter_operator", "64MiB", 64 << 20}, {"larger_operator", "512MiB", ServerGoMemoryLimit}} {
		t.Run(c.name, func(t *testing.T) {
			command := exec.Command(executable, "-test.run=^TestNHA165ServerMemoryBudgetChild$", "-test.count=1")
			for _, v := range os.Environ() {
				if !strings.HasPrefix(v, "GOMEMLIMIT=") && !strings.HasPrefix(v, "HOMEWARD_NHA165_BUDGET_CHILD=") {
					command.Env = append(command.Env, v)
				}
			}
			if c.value != "" {
				command.Env = append(command.Env, "GOMEMLIMIT="+c.value)
			}
			command.Env = append(command.Env, "HOMEWARD_NHA165_BUDGET_CHILD="+strconv.FormatInt(c.want, 10))
			output, e := command.CombinedOutput()
			if e != nil {
				t.Fatalf("real budget child: %v: %s", e, output)
			}
			if !strings.Contains(string(output), "actual_server_memory_budget="+strconv.FormatInt(c.want, 10)) {
				t.Fatalf("missing actual child budget: %s", output)
			}
		})
	}
}
func TestNHA165ServerMemoryBudgetChild(t *testing.T) {
	raw := os.Getenv("HOMEWARD_NHA165_BUDGET_CHILD")
	if raw == "" {
		t.Skip("owned subprocess only")
	}
	want, e := strconv.ParseInt(raw, 10, 64)
	if e != nil {
		t.Fatal(e)
	}
	effective := ConfigureServer()
	if effective != want || debug.SetMemoryLimit(-1) != want {
		t.Fatalf("effective limit=%d runtime=%d want=%d", effective, debug.SetMemoryLimit(-1), want)
	}
	if again := ConfigureServer(); again != want || debug.SetMemoryLimit(-1) != want {
		t.Fatal("repeated startup changed stricter effective limit")
	}
	fmt.Printf("actual_server_memory_budget=%d\n", effective)
}
