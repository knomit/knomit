// Custom SQLite functions registered via ConnectHook. These run inside the
// database engine and are used by bulk SQL statements (e.g. fast rebuild).
package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// registerSQLFuncs registers all custom SQL functions on a new connection.
// Called via ConnectHook so every pooled connection gets the same functions.
func registerSQLFuncs(conn *sqlite3.SQLiteConn) error {
	if err := conn.RegisterFunc("knomit_parse_fact", sqlParseFact, true); err != nil {
		return err
	}
	return conn.RegisterFunc("knomit_cosine_sim", sqlCosineSim, true)
}

// parsedFact is the JSON structure returned by knomit_parse_fact.
type parsedFact struct {
	Title      string   `json:"title"`
	Type       string   `json:"type"`
	Domain     []string `json:"domain"`
	Entities   []string `json:"entities"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
	Refs       []string `json:"refs"`
}

// sqlParseFact parses a knomit fact markdown blob (YAML frontmatter + body)
// and returns a JSON string, or nil (SQL NULL) for non-fact files.
// The parsing logic mirrors parseFact in parse.go exactly.
func sqlParseFact(data []byte) interface{} {
	content := string(data)

	// Split on "---" delimiters to separate frontmatter from body.
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return nil
	}

	frontmatter := parts[1]
	body := strings.TrimSpace(parts[2])

	// Parse each key: value line in the frontmatter.
	var domain []string
	var entities []string
	var refs []string
	var confidence float64
	var sources int
	var factType string

	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)

		switch k {
		case "type":
			factType = v
		case "domain":
			domain = parseYAMLList(v)
		case "entities":
			entities = parseYAMLList(v)
		case "refs":
			refs = parseYAMLList(v)
		case "confidence":
			fmt.Sscanf(v, "%f", &confidence)
		case "sources":
			fmt.Sscanf(v, "%d", &sources)
		}
	}

	if factType == "" {
		factType = "observation"
	}

	// Extract title from the first markdown heading line.
	title := ""
	if strings.HasPrefix(body, "#") {
		nl := strings.IndexByte(body, '\n')
		if nl < 0 {
			title = strings.TrimSpace(strings.TrimLeft(body, "#"))
		} else {
			title = strings.TrimSpace(body[:nl])
			title = strings.TrimSpace(strings.TrimLeft(title, "#"))
		}
	}

	if title == "" {
		return nil
	}

	if domain == nil {
		domain = []string{}
	}
	if entities == nil {
		entities = []string{}
	}
	if refs == nil {
		refs = []string{}
	}

	pf := parsedFact{
		Title:      title,
		Type:       factType,
		Domain:     domain,
		Entities:   entities,
		Confidence: confidence,
		Sources:    sources,
		Refs:       refs,
	}

	b, err := json.Marshal(pf)
	if err != nil {
		return nil
	}
	return string(b)
}

// sqlCosineSim computes cosine similarity between two embedding BLOBs.
// Both inputs must be little-endian float32 arrays of equal length.
// Returns a REAL in [-1, 1] or NULL on invalid input.
func sqlCosineSim(a, b []byte) interface{} {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) || len(a)%4 != 0 {
		return nil
	}
	n := len(a) / 4
	var dot, normA, normB float64
	for i := 0; i < n; i++ {
		va := float64(math.Float32frombits(binary.LittleEndian.Uint32(a[i*4:])))
		vb := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:])))
		dot += va * vb
		normA += va * va
		normB += vb * vb
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return nil
	}
	return dot / denom
}
