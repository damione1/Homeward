package server

import auth "github.com/damione1/Homeward/gen/home/auth/v1"

// Exact protected routes shared by the business resource families.
type businessRoute struct {
	path string
	op   auth.ScopeOperation
	flow RouteFlow
}
