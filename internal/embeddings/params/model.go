package params

// DefaultModelID is the shipped default embedding model (validated best on the
// knomit corpus).
//
// It lives here rather than beside the model registry in internal/embeddings so
// that internal/config can reference it directly. The registry carries cgo, so
// config cannot import it; before this constant moved down, config.Defaults()
// duplicated the literal "embeddinggemma" and a comment asking the next reader
// to keep the two in sync was the only thing holding them together.
const DefaultModelID = "embeddinggemma"
