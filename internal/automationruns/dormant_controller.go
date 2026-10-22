package automationruns

// NewDormantController prepares the existing concrete interpreter/controller
// before an initial configuration contains any executable definitions. It can
// only admit an empty plan and the same real interpreter in both roles.
func NewDormantController(plan Plan, reader ResultReader, interpreter *Interpreter) (*Controller, error) {
	if !plan.Valid() || len(plan.items)+len(plan.scripts) != 0 || interpreter == nil {
		return nil, ErrInvalidPlan
	}
	return newController(plan, reader, interpreter, interpreter, true)
}
