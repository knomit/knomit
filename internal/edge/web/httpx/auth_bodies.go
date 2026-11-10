package httpx

// The request bodies of the unified-auth management routes (plan P3, Task
// 3.5; /api/v1/auth/*). The handlers decode into these types and `knomit
// auth` encodes them, so the server and the CLI read one shape. Their route
// permission (auth:manage) does not depend on the body: no resolver reads
// them.

// AuthBodyMaxBytes bounds every /api/v1/auth/* body: they are a few short
// strings.
const AuthBodyMaxBytes = 4 << 10

// AuthTokenCreateBody is POST /auth/tokens: a personal access token with one
// role (reader, contributor or operator), a label, and an expiry as a
// lifetime from now ("90d", "12h", "30m"; empty means the server's
// [auth].pat_default_ttl, and more than [auth].pat_max_ttl is refused).
type AuthTokenCreateBody struct {
	Role      string `json:"role"`
	Label     string `json:"label"`
	ExpiresIn string `json:"expires_in"`
}

// AuthBindingBody is POST /auth/bindings: bind a principal (its one spelling,
// e.g. os:uid:1000 or anonymous) to a role. Token principals are bound when
// the token is minted or approved, never here.
type AuthBindingBody struct {
	Principal string `json:"principal"`
	Role      string `json:"role"`
	Label     string `json:"label"`
}
