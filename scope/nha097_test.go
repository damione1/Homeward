package scope

import (
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA097ButtonPolicyExplicitDecisions(t *testing.T) {
	policy, err := RetainedHelperButtonPolicy()
	if err != nil {
		t.Fatal(err)
	}
	for aliasText, want := range map[string]HelperButtonDecision{
		"input_button.cast_to_nest_hub":        HelperButtonNoAuthorizedTarget,
		"input_button.pause_bird_s_radio":      HelperButtonLocalPress,
		"input_button.start_bird_s_radio":      HelperButtonLocalPress,
		"input_button.trash_out":               HelperButtonLocalPress,
		"input_button.future_valid_definition": HelperButtonLocalPress,
	} {
		alias, err := domain.ParseHAAlias(aliasText)
		if err != nil {
			t.Fatal(err)
		}
		if got := policy.Decision(alias); got != want {
			t.Fatalf("%s: got %d, want %d", aliasText, got, want)
		}
	}
	for _, mutant := range []string{
		strings.Replace(string(helperButtonPolicyV1), "no_authorized_target", "local_press", 1) + " {}",
		strings.Replace(string(helperButtonPolicyV1), "input_button.trash_out", "input_button.cast_to_nest_hub", 1),
		strings.Replace(string(helperButtonPolicyV1), "local_press", "dispatch_cast", 1),
	} {
		if _, err := ParseHelperButtonPolicy([]byte(mutant)); err == nil {
			t.Fatal("invalid policy admitted")
		}
	}
}
