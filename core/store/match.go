package store

import (
	"strings"

	"github.com/can3p/tommy/core/event"
)

// Matches reports whether e satisfies the filters of q. Limit and Offset are
// paging concerns and are ignored here, so implementations can share this.
func (q Query) Matches(e *event.Event) bool {
	if q.Plugin != "" && e.Plugin != q.Plugin {
		return false
	}
	if q.Provider != "" && e.Provider != q.Provider {
		return false
	}
	if q.Type != "" && e.Type != q.Type {
		return false
	}
	if !q.Since.IsZero() && !e.ReceivedAt.After(q.Since) {
		return false
	}
	if q.Search != "" && !matchesSearch(e, q.Search) {
		return false
	}
	return true
}

func matchesSearch(e *event.Event, search string) bool {
	needle := strings.ToLower(search)
	if strings.Contains(strings.ToLower(e.Summary.Title), needle) ||
		strings.Contains(strings.ToLower(e.Summary.Snippet), needle) ||
		strings.Contains(strings.ToLower(e.Summary.From), needle) ||
		strings.Contains(strings.ToLower(e.Type), needle) {
		return true
	}
	for _, to := range e.Summary.To {
		if strings.Contains(strings.ToLower(to), needle) {
			return true
		}
	}
	return false
}
