// Package pathmatch matches request paths against catalog patterns.
package pathmatch

import "strings"

// Match compares segment by segment; a {placeholder} segment matches any one
// segment. Query strings are not part of the path.
func Match(pattern, path string) bool {
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	xs := strings.Split(strings.Trim(path, "/"), "/")
	if len(ps) != len(xs) {
		return false
	}
	for i := range ps {
		if ps[i] != xs[i] && !(strings.HasPrefix(ps[i], "{") && strings.HasSuffix(ps[i], "}") && xs[i] != "") {
			return false
		}
	}
	return true
}
