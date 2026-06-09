// Package hal provides HAL (Hypertext Application Language, application/hal+json)
// serialization primitives for knomit's REST API.
//
// HAL encodes hypermedia controls under a `_links` map. Each link is a Link
// struct with an href (required) and optional flags. This package provides
// just the envelope types; domain-specific view types (e.g. FactView) live
// in their own files and use these primitives.
package hal

// Link is a single HAL link. Href is required; Templated signals an RFC 6570
// URI template so clients know to substitute variables.
type Link struct {
	Href      string `json:"href"`
	Templated bool   `json:"templated,omitempty"`
}

// LinkMap is the shape of the _links object in a HAL response.
type LinkMap map[string]Link

// Clean returns a copy of the map with zero-value links removed. Useful when
// building link maps with conditional entries.
func (m LinkMap) Clean() LinkMap {
	out := make(LinkMap, len(m))
	for k, v := range m {
		if v.Href == "" {
			continue
		}
		out[k] = v
	}
	return out
}
