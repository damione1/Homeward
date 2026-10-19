package config

import (
	"fmt"

	"github.com/damione1/Homeward/internal/recorder"
)

// RecorderInput is the strict YAML boundary. A missing block or field takes
// the admitted default; an explicit zero is invalid rather than absent.
type RecorderInput struct {
	BudgetMiB *int `yaml:"budget_mib"`
}

func ParseRecorderInput(input *RecorderInput) (recorder.Limits, error) {
	budget := recorder.DefaultBudgetBytes
	if input != nil && input.BudgetMiB != nil {
		if *input.BudgetMiB < recorder.MinBudgetBytes/(1<<20) || *input.BudgetMiB > recorder.MaxBudgetBytes/(1<<20) {
			return recorder.Limits{}, fmt.Errorf("configuration: recorder budget_mib must be between 16 and 32")
		}
		budget = *input.BudgetMiB * (1 << 20)
	}
	limits, err := recorder.NewLimits(budget)
	if err != nil {
		return recorder.Limits{}, fmt.Errorf("configuration: %w", err)
	}
	return limits, nil
}
