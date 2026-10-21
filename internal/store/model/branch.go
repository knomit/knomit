package model

// Branch holds metadata about an indexed branch.
type Branch struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	GitRef string `json:"git_ref"`
}
