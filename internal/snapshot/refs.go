package snapshot

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ErrScopeExists is returned by Fork when the target scope ref
// already exists: Fork creates a scope, it never repoints one —
// a scope name may be cited by other work, so moving it silently
// would rewrite what those citations name.
var ErrScopeExists = errors.New("snapshot: fork scope already exists")

// ErrScopeNotFound is returned by the scope operations when the
// named scope ref does not exist: a name that matches no ref —
// including a name the scope-name law rejects outright, which can
// name no scope — is an absence, not a different kind of failure.
var ErrScopeNotFound = errors.New("snapshot: scope not found")

// ErrScopeHeadMoved is returned by AdvanceScope when the scope's
// current head differs from the caller's expected old value: the
// compare-and-swap refused, and the head is left where it was.
var ErrScopeHeadMoved = errors.New("snapshot: scope head moved")

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
// written, so a rejected fork creates nothing. A scope that
// already exists is refused with ErrScopeExists and left
// untouched: the ref is written with a create-only update-ref
// (zero old value), so the refusal is atomic, not check-then-write.
func (s *Store) Fork(scope, commitID string) error {
	if !scopeNamePattern.MatchString(scope) {
		return fmt.Errorf("snapshot: fork: invalid scope name %q (must match %s)", scope, scopeNamePattern)
	}
	if _, err := s.git(nil, "rev-parse", "--verify", "--quiet", commitID+"^{commit}"); err != nil {
		return fmt.Errorf("snapshot: fork: %q does not name a commit: %w", commitID, err)
	}
	ref := scopesRefPrefix + scope
	if _, err := s.git(nil, "rev-parse", "--verify", "--quiet", ref); err == nil {
		return fmt.Errorf("%w: %q", ErrScopeExists, scope)
	}
	if _, err := s.git(nil, "update-ref", ref, commitID, zeroOID); err != nil {
		// The create-only write failed: a peer created the ref
		// after the check above, or the write itself failed.
		// Distinguish the two so an existing scope always reports
		// as ErrScopeExists.
		if _, cerr := s.git(nil, "rev-parse", "--verify", "--quiet", ref); cerr == nil {
			return fmt.Errorf("%w: %q", ErrScopeExists, scope)
		}
		return fmt.Errorf("snapshot: fork: update-ref: %w", err)
	}
	return nil
}

// ScopeHead returns the commit refs/vcscore/scopes/<scope>
// names, or ErrScopeNotFound when no such scope exists.
func (s *Store) ScopeHead(scope string) (string, error) {
	if !scopeNamePattern.MatchString(scope) {
		return "", fmt.Errorf("%w: %q", ErrScopeNotFound, scope)
	}
	head, err := s.git(nil, "rev-parse", "--verify", "--quiet", scopesRefPrefix+scope+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrScopeNotFound, scope)
	}
	return head, nil
}

// AdvanceScope moves refs/vcscore/scopes/<scope> from oldCommit
// to newCommit as a compare-and-swap: the ref is rewritten only
// if its current head is exactly oldCommit, so two advancers
// racing one scope cannot both succeed — the loser is refused
// with ErrScopeHeadMoved and the head stays where the winner put
// it. A scope that does not exist is ErrScopeNotFound, and
// newCommit must name a commit; both are established before the
// write, so a refused advance changes nothing.
func (s *Store) AdvanceScope(scope, oldCommit, newCommit string) error {
	if _, err := s.git(nil, "rev-parse", "--verify", "--quiet", newCommit+"^{commit}"); err != nil {
		return fmt.Errorf("snapshot: advance scope %q: %q does not name a commit: %w", scope, newCommit, err)
	}
	head, err := s.ScopeHead(scope)
	if err != nil {
		return err
	}
	if head != oldCommit {
		return fmt.Errorf("%w: scope %q is at %s, not %s", ErrScopeHeadMoved, scope, head, oldCommit)
	}
	if _, err := s.git(nil, "update-ref", scopesRefPrefix+scope, newCommit, oldCommit); err != nil {
		// The swap lost a race between the check above and the
		// write: re-read and classify, so a moved head always
		// reports as ErrScopeHeadMoved rather than a raw
		// plumbing failure.
		now, rerr := s.ScopeHead(scope)
		if rerr != nil {
			return rerr
		}
		if now != oldCommit {
			return fmt.Errorf("%w: scope %q is at %s, not %s", ErrScopeHeadMoved, scope, now, oldCommit)
		}
		return fmt.Errorf("snapshot: advance scope %q: update-ref: %w", scope, err)
	}
	return nil
}

// ArchiveScope retires a scope without deleting its history: it
// creates refs/vcscore/archive/<scope> at the scope's head
// (create-only — an existing archive of the same name is never
// an overwrite) and then deletes refs/vcscore/scopes/<scope>.
// The archived commit stays fully readable through the tree
// surface; archiving is a move, not a deletion (AC-F6-5).
//
// The create and the delete are separate operations, so a
// process that dies between them — or a delete that fails, e.g.
// on a stale ref lock — leaves both refs standing. That state
// is resumable, and the heads decide which kind it is: if the
// archive ref names the scope's current head, the standing
// archive is this archival's own half-done work, and a retry
// completes it at the delete; if it names a different commit,
// the name was archived at another head — a genuine conflict,
// refused descriptively with nothing moved. No state a crash
// can leave behind wedges a retry permanently.
func (s *Store) ArchiveScope(scope string) error {
	head, err := s.ScopeHead(scope)
	if err != nil {
		return err
	}
	archiveRef := archiveRefPrefix + scope
	existing, aerr := s.git(nil, "rev-parse", "--verify", "--quiet", archiveRef+"^{commit}")
	switch {
	case aerr == nil && existing != head:
		return fmt.Errorf("snapshot: archive scope %q: archive ref %s already exists at %s, but the scope's head is %s", scope, archiveRef, existing, head)
	case aerr == nil:
		// The archive already names this scope's head: a
		// previous ArchiveScope died after the create but
		// before the delete (or the delete failed). The
		// archival is half-done, not conflicted — fall
		// through to the delete and complete it.
	default:
		if _, err := s.git(nil, "update-ref", archiveRef, head, zeroOID); err != nil {
			if _, cerr := s.git(nil, "rev-parse", "--verify", "--quiet", archiveRef); cerr == nil {
				return fmt.Errorf("snapshot: archive scope %q: archive ref %s already exists", scope, archiveRef)
			}
			return fmt.Errorf("snapshot: archive scope %q: update-ref: %w", scope, err)
		}
	}
	if _, err := s.git(nil, "update-ref", "-d", scopesRefPrefix+scope, head); err != nil {
		return fmt.Errorf("snapshot: archive scope %q: delete scope ref: %w", scope, err)
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
