package casedef

import "strings"

// KnownTags is the controlled vocabulary scenario tags are drawn from.
//
// Tags are the library's real taxonomy — a scenario is about gap locks and
// insert intentions, not about the folder it happens to sit in — and a taxonomy
// only works if everyone spells things the same way. Left uncontrolled it grew
// synonyms (`mdl` and `metadata-lock`, `range` and `range-scan`) and a long
// tail of descriptions used once, which is a list nobody can filter by.
//
// An unknown tag is a lint warning rather than an error: a scenario you wrote
// for yourself is allowed its own vocabulary. The warning exists so the shipped
// library does not drift back.
var KnownTags = map[string]string{
	// What InnoDB takes.
	"record-lock":      "lock",
	"gap-lock":         "lock",
	"next-key-lock":    "lock",
	"insert-intention": "lock",
	"shared-lock":      "lock",
	"exclusive-lock":   "lock",
	"intention-lock":   "lock",
	"table-lock":       "lock",
	"metadata-lock":    "lock",
	"supremum":         "lock",

	// What you wrote.
	"for-update":  "statement",
	"for-share":   "statement",
	"nowait":      "statement",
	"skip-locked": "statement",
	"lock-tables": "statement",
	"savepoint":   "statement",
	"insert":      "statement",
	"delete":      "statement",
	"join":        "statement",
	"order-by":    "statement",
	"range-scan":  "statement",
	"ddl":         "statement",

	// What is going on underneath.
	"lock-upgrade":          "concept",
	"lock-ordering":         "concept",
	"lock-compatibility":    "concept",
	"lock-scope":            "concept",
	"mvcc":                  "concept",
	"consistent-read":       "concept",
	"current-read":          "concept",
	"semi-consistent-read":  "concept",
	"phantom":               "concept",
	"lost-update":           "concept",
	"deadlock-detection":    "concept",
	"head-of-line-blocking": "concept",

	// Isolation.
	"isolation":       "isolation",
	"read-committed":  "isolation",
	"repeatable-read": "isolation",
	"serializable":    "isolation",

	// Schema and indexes.
	"clustered-index": "schema",
	"secondary-index": "schema",
	"unique-index":    "schema",
	"missing-index":   "schema",
	"full-scan":       "schema",
	"foreign-key":     "schema",
	"cascade":         "schema",
	"partitioning":    "schema",

	// How it ended.
	"deadlock":   "outcome",
	"timeout":    "outcome",
	"error-1213": "outcome",
	"error-1205": "outcome",
	"error-3572": "outcome",

	// The wire.
	"wire-protocol":       "protocol",
	"binary-protocol":     "protocol",
	"prepared-statements": "protocol",

	// Shapes people recognise.
	"queue":         "pattern",
	"uuidv7":        "pattern",
	"duplicate-key": "pattern",
}

// UnknownTags returns the tags on a case that are not in the vocabulary.
func UnknownTags(c *Case) []string {
	var out []string
	for _, t := range c.Tags {
		if _, ok := KnownTags[strings.ToLower(strings.TrimSpace(t))]; !ok {
			out = append(out, t)
		}
	}
	return out
}
