// Package authz provides the two-layer authorization model (docs/02-architecture.md §5).
// Layer A authenticates service API keys; Layer B resolves per-request authorization
// against Postgres. Both layers are stubs in M0; full implementation lands in M1.
package authz

// PrincipalType identifies how a request was authenticated.
type PrincipalType string

const (
	// PrincipalTypeService is an opaque bearer service API key (backoffice, Layer A).
	PrincipalTypeService PrincipalType = "service"

	// PrincipalTypeSession is a short-lived session token for the future POC frontend.
	// The seam is reserved here; the session issuer is built in POC-FE (§5.3).
	PrincipalTypeSession PrincipalType = "session"
)

// Principal represents an authenticated caller after Layer A passes.
// It is injected into the Gin context by the auth middleware.
type Principal struct {
	Type     PrincipalType
	ID       string // api_key.id for service principals
	IsAdmin  bool   // admin roster-management privilege (Layer B)
	KeyScope string // e.g. "backoffice"
}
