package configplans

import "testing"

func TestNHA223DisplayProfilePlanHasOnlyConfigurationEffect(t *testing.T) {
	before := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", "{}\n"))
	for _, raw := range []string{"display_profile: {time_zone: Europe/Paris}\n", "display_profile: {time_zone: UTC}\n", "{}\n"} {
		after := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", raw))
		changes := difference(before, after)
		if len(changes) != 1 || changes[0].Field() != "display_profile" || changes[0].Effect() != ConfigurationEffect || changes[0].ReferencesAdded() != 0 || changes[0].ReferencesRemoved() != 0 || before.digest == after.digest {
			t.Fatal("presentation lost its configuration effect or acquired object authority")
		}
		before = after
	}
}
