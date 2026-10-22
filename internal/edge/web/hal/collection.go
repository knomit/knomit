package hal

// CollectionView is the HAL envelope for a paged collection of items of type T.
//
//	{
//	  "count": 147,
//	  "_links": { "self": {...}, "next": {...}, "prev": {...}, "first": {...} },
//	  "_embedded": { "<rel>": [ item, item, ... ] }
//	}
//
// The Embedded map key is the relation name (e.g. "repos", "facts") — callers
// choose a plural, stable name per collection kind.
type CollectionView[T any] struct {
	Count    int            `json:"count"`
	Links    LinkMap        `json:"_links"`
	Embedded map[string][]T `json:"_embedded"`
}
