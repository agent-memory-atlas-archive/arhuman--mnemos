package security

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
)

// Accept declares one scanner finding that is known not to be a secret. Entries
// are for non-secret placeholders only (all-zero tokens, documented example
// keys), never for real credentials. One entry suppresses one finding shape, at
// the paths it lists, never a whole file: any other finding in the same file is
// still reported.
//
// Rule is the rule name the finding must carry. For generic-credential-assignment,
// Identifier is the assigned keyword (case-insensitive, e.g. "token") and Value
// the quoted value; for any other rule, Identifier is empty and Value is the
// whole matched substring. Paths are doublestar globs over the slash-separated,
// tree-root-relative uri. Reason records why the value is safe.
type Accept struct {
	Rule       string   `koanf:"rule"`
	Identifier string   `koanf:"identifier"`
	Value      string   `koanf:"value"`
	Paths      []string `koanf:"paths"`
	Reason     string   `koanf:"reason"`
}

// genericKeywordRe matches a whole identifier against the generic rule's
// keyword set.
var genericKeywordRe = regexp.MustCompile(`(?i)^(?:` + genericKeywords + `)$`)

// genericMinValueLen is the shortest value the generic rule matches.
const genericMinValueLen = 16

// AcceptingScanner wraps a base SecretScanner and drops the findings that an
// Accept entry declares safe. Construct with NewScanner, after the entries have
// passed ValidateAccepts.
type AcceptingScanner struct {
	base    SecretScanner
	accepts []Accept
}

// NewScanner returns the configured SecretScanner: the plain RegexScanner when
// accepts is empty (no filtering overhead), otherwise an AcceptingScanner over
// it. accepts must have passed ValidateAccepts; it is not re-validated here.
func NewScanner(accepts []Accept) SecretScanner {
	if len(accepts) == 0 {
		return NewRegexScanner()
	}

	return AcceptingScanner{base: NewRegexScanner(), accepts: accepts}
}

// Scan runs the base scanner and drops each finding matched by an Accept entry
// on rule, value and path. A pathless scan (path == "") is never exempt. The
// result is nil when no finding survives.
func (s AcceptingScanner) Scan(path, content string) ([]Finding, error) {
	findings, err := s.base.Scan(path, content)
	if err != nil {
		return nil, err
	}
	var kept []Finding
	for _, f := range findings {
		if !s.accepted(path, f) {
			kept = append(kept, f)
		}
	}

	return kept, nil
}

func (s AcceptingScanner) accepted(path string, f Finding) bool {
	if path == "" {
		return false
	}
	for _, a := range s.accepts {
		if f.Rule == a.Rule && valueMatches(a, f) && pathMatches(a.Paths, path) {
			return true
		}
	}

	return false
}

func valueMatches(a Accept, f Finding) bool {
	if a.Identifier != "" {
		return strings.EqualFold(f.identifier, a.Identifier) && f.value == a.Value
	}

	return f.secret == a.Value
}

func pathMatches(globs []string, path string) bool {
	for _, g := range globs {
		// Globs are checked by ValidateAccepts, so a pattern error means no match.
		if ok, _ := doublestar.Match(g, path); ok {
			return true
		}
	}

	return false
}

// ValidateAccepts checks every entry and returns the first problem, prefixed
// "security: accept #<1-based index>: ". It rejects an unknown rule, an empty
// value or reason, no paths, an invalid glob, a duplicate entry, and any entry
// that could never match a finding of its rule: for generic-credential-assignment
// an identifier outside the keyword set or a value shorter than 16 characters or
// holding a quote; for other rules an identifier, or a value the rule does not
// match in full. It does not modify accepts.
func ValidateAccepts(accepts []Accept) error {
	type key struct{ rule, identifier, value, paths string }
	seen := make(map[key]bool, len(accepts))
	for i, a := range accepts {
		if err := validateAccept(a); err != nil {
			return fmt.Errorf("security: accept #%d: %w", i+1, err)
		}
		k := key{a.Rule, strings.ToLower(a.Identifier), a.Value, strings.Join(a.Paths, "\x00")}
		if seen[k] {
			return fmt.Errorf("security: accept #%d: duplicate entry", i+1)
		}
		seen[k] = true
	}

	return nil
}

func validateAccept(a Accept) error {
	idx := slices.IndexFunc(rules, func(r rule) bool { return r.name == a.Rule })
	if idx < 0 {
		return fmt.Errorf("unknown rule %q", a.Rule)
	}
	if strings.TrimSpace(a.Value) == "" {
		return errors.New("empty value")
	}
	if strings.TrimSpace(a.Reason) == "" {
		return errors.New("empty reason")
	}
	if len(a.Paths) == 0 {
		return errors.New("no paths")
	}
	for _, p := range a.Paths {
		if !doublestar.ValidatePattern(p) {
			return fmt.Errorf("invalid path glob %q", p)
		}
	}
	if a.Rule == genericRule {
		return validateGenericAccept(a)
	}
	if a.Identifier != "" {
		return fmt.Errorf("identifier is only valid for %s", genericRule)
	}
	if rules[idx].re.FindString(a.Value) != a.Value {
		return fmt.Errorf("value is not a full match of rule %q", a.Rule)
	}

	return nil
}

func validateGenericAccept(a Accept) error {
	if !genericKeywordRe.MatchString(a.Identifier) {
		return fmt.Errorf("identifier %q is not a %s keyword", a.Identifier, genericRule)
	}
	if utf8.RuneCountInString(a.Value) < genericMinValueLen {
		return fmt.Errorf("value shorter than %d characters", genericMinValueLen)
	}
	if strings.ContainsAny(a.Value, `'"`) {
		return errors.New("value contains a quote")
	}

	return nil
}
