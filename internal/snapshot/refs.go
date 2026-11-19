package snapshot

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Refs layout law (AC-F4-5): every ref in the store lives under
// refs/vcscore/, with second component one of ground, materialized,
// scopes, ops, or archive. The snapshot chain lives under ops/ (one
// ops/snapshot-<prefix> ref per commit, ops/latest as the chain
// head); no ground ref is written — the chain is operational
// history, not a world line (plan Task 15 decision). The enumerable
// classes below are the ones ListScopes reports; ground and
// materialized name no Phase 1 refs.
const (
	scopesRefPrefix  = "refs/vcscore/scopes/"
	archiveRefPrefix = "refs/vcscore/archive/"
)

// RefKind is the layout class a ref belongs to: "scope", "ops", or
// "archive".
type RefKind string

const (
	RefScope   RefKind = "scope"
	RefOps     RefKind = "ops"
	RefArchive RefKind = "archive"
)

// ScopeRef is one enumerated ref: its layout class, its name (the
// ref's path under its class, e.g. "lane-a", "latest",
// "snapshot-<prefix>"), and the commit it points at.
type ScopeRef struct {
	Kind     RefKind
	Name     string
	CommitID string
}

// refClasses maps each enumerable layout class's ref prefix to its
// kind.
var refClasses = []struct {
	prefix string
	kind   RefKind
}{
	{scopesRefPrefix, RefScope},
	{opsRefPrefix, RefOps},
	{archiveRefPrefix, RefArchive},
}

// scopeNamePattern is the scope-name law: lowercase alphanumerics
// and hyphens, starting with an alphanumeric — a single ref path
// component, so a scope can never escape refs/vcscore/scopes/.
var scopeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Fork creates the scope ref refs/vcscore/scopes/<scope> pointing
// at commitID. It is zero-copy: a single update-ref, no new objects
// (AC-F4-3). The scope name must match the scope-name law and
// commitID must name a commit; both are validated before the ref is
// written, so a rejected fork creates nothing.
func (s *Store) Fork(scope, commitID string) error {
	if !scopeNamePattern.MatchString(scope) {
		return fmt.Errorf("snapshot: fork: invalid scope name %q (must match %s)", scope, scopeNamePattern)
	}
	if _, err := s.git(nil, "rev-parse", "--verify", "--quiet", commitID+"^{commit}"); err != nil {
		return fmt.Errorf("snapshot: fork: %q does not name a commit: %w", commitID, err)
	}
	if _, err := s.git(nil, "update-ref", scopesRefPrefix+scope, commitID); err != nil {
		return fmt.Errorf("snapshot: fork: update-ref: %w", err)
	}
	return nil
}

// ListScopes enumerates every ref under the layout's enumerable
// classes — refs/vcscore/scopes/, refs/vcscore/ops/, and
// refs/vcscore/archive/ — each returned with its kind, sorted by
// (kind, name). Despite the name it is not a scopes-only listing:
// it is the layout enumeration AC-F4-5 checks against.
func (s *Store) ListScopes() ([]ScopeRef, error) {
	out, err := s.git(nil, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, fmt.Errorf("snapshot: list scopes: %w", err)
	}
	var refs []ScopeRef
	if out != "" {
		for _, line := range strings.Split(out, "\n") {
			refname, oid, ok := strings.Cut(line, " ")
			if !ok {
				return nil, fmt.Errorf("snapshot: list scopes: malformed for-each-ref line %q", line)
			}
			for _, class := range refClasses {
				if strings.HasPrefix(refname, class.prefix) {
					refs = append(refs, ScopeRef{
						Kind:     class.kind,
						Name:     strings.TrimPrefix(refname, class.prefix),
						CommitID: oid,
					})
					break
				}
			}
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].Name < refs[j].Name
	})
	return refs, nil
}
