package main

// Scope, repository filters and cursors for the console lists (#345, docs/console-ux.md §9):
//   - repo=owner/name keeps the items of that repository;
//   - scope=mine keeps the items of repositories where the caller has write access on the forge (also for admins, so
//     an admin can look at "my projects" too); scope=all (default) keeps everything the caller may see;
//   - limit and cursor page a list. The cursor is opaque; lists without a stable id use an offset inside it.
// A filter only narrows: it never shows what the visibility rule of the list hides.

import (
	"context"
	"errors"
	"net/url"
	"strconv"
)

type scope struct {
	repoFilter string
	mine       bool
	admin      bool
	may        func(forge, repo string) bool // caller has write access (cached per request)
}

// repo reports whether items of forge/repo pass the filters.
func (s scope) repo(forge, repo string) bool {
	if s.repoFilter != "" && s.repoFilter != repo {
		return false
	}
	if s.mine && !s.may(forge, repo) {
		return false
	}
	return true
}

// visible: the caller may see items of forge/repo (admin group, or write access) and they pass the filters.
func (s scope) visible(forge, repo string) bool {
	return (s.admin || s.may(forge, repo)) && s.repo(forge, repo)
}

// agent: an agent passes when one of its repositories passes.
func (s scope) agent(a agent) bool {
	if s.repoFilter == "" && !s.mine {
		return true
	}
	for _, r := range a.Repos {
		if s.repo(a.Forge, r) {
			return true
		}
	}
	return false
}

// scopeOf reads repo and scope from the query.
func (w *writer) scopeOf(ctx context.Context, id *identity, q url.Values) (scope, error) {
	s := scope{repoFilter: q.Get("repo")}
	if s.repoFilter != "" && !validRepo(s.repoFilter) {
		return s, errors.New("repo must be owner/name")
	}
	switch q.Get("scope") {
	case "", "all":
	case "mine":
		s.mine = true
	default:
		return s, errors.New("scope: mine or all")
	}
	s.may = w.writeAccess(ctx, id)
	s.admin = id != nil && w.isAdmin(id)
	return s, nil
}

// writeAccess returns a per-request cached check of the caller's write access (fail closed on forge errors).
func (w *writer) writeAccess(ctx context.Context, id *identity) func(forge, repo string) bool {
	cache := map[string]bool{}
	return func(forge, repo string) bool {
		if id == nil {
			return false
		}
		key := forge + "\x00" + repo
		if v, ok := cache[key]; ok {
			return v
		}
		p, err := w.repoPerm(ctx, id, forge, repo)
		cache[key] = err == nil && p >= permWrite
		return cache[key]
	}
}

// listPage is limit + offset cursor for lists built in memory.
type listPage struct {
	limit, offset int
}

func parsePage(q url.Values, def int) (listPage, error) {
	p := listPage{limit: def}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 || !digitsRe.MatchString(v) {
			return p, errors.New("limit must be 1..500")
		}
		p.limit = n
	}
	if v := q.Get("cursor"); v != "" {
		n, err := decodeCursor(v)
		if err != nil || n > 1<<20 {
			return p, errors.New("bad cursor")
		}
		p.offset = int(n)
	}
	return p, nil
}

// slice returns [from, to) for a list of n items and the cursor of the next page ("" = last page).
func (p listPage) slice(n int) ([2]int, string) {
	from := min(p.offset, n)
	to := min(from+p.limit, n)
	next := ""
	if to < n {
		next = encodeCursor(int64(to))
	}
	return [2]int{from, to}, next
}
