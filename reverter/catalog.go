package main

// The compensation catalog: maps method+path to a tier and, for tier 1, the
// inverse call template. Tier 0 is deferred, 1 invertible, 3 uncompensatable;
// an unmatched call is tier 3.

import (
	"atomicfaas/pathmatch"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

type inverse struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
}

type rule struct {
	Match struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	} `json:"match"`
	Tier    int      `json:"tier"`
	Inverse *inverse `json:"inverse"`
}

// Unmatched is tier 3. A logged tier-0 record executed instead of being
// deferred, so it floors as well.
func effectiveTier(r *rule) int {
	if r == nil || r.Tier == 0 {
		return 3
	}
	return r.Tier
}

type catalog []rule

func loadCatalog(path string) (catalog, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c catalog
	if err := json.Unmarshal(buf, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, r := range c {
		switch {
		case r.Tier != 0 && r.Tier != 1 && r.Tier != 3:
			return nil, fmt.Errorf("%s: %s %s: tier %d is not 0, 1 or 3", path, r.Match.Method, r.Match.Path, r.Tier)
		case r.Tier == 1 && r.Inverse == nil:
			return nil, fmt.Errorf("%s: %s %s: tier 1 without an inverse", path, r.Match.Method, r.Match.Path)
		}
	}
	return c, nil
}

func (c catalog) match(method, rawURL string) *rule {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	for i := range c {
		if c[i].Match.Method == method && pathmatch.Match(c[i].Match.Path, u.Path) {
			return &c[i]
		}
	}
	return nil
}

// Tokens: {body.<field>} of the request, {resp.<field>} of the recorded response.

var token = regexp.MustCompile(`\{(body\.\w+|resp\.\w+)\}`)

func lookupToken(name string, e Effect, respBody string) (any, error) {
	switch {
	case strings.HasPrefix(name, "body."):
		return jsonField(e.Body, strings.TrimPrefix(name, "body."))
	default:
		return jsonField(respBody, strings.TrimPrefix(name, "resp."))
	}
}

func jsonField(doc, field string) (any, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		return nil, fmt.Errorf("not a JSON object: %.80q", doc)
	}
	v, ok := m[field]
	if !ok {
		return nil, fmt.Errorf("no field %q in %.80q", field, doc)
	}
	return v, nil
}

func renderString(tmpl string, e Effect, respBody string) (string, error) {
	var firstErr error
	out := token.ReplaceAllStringFunc(tmpl, func(tok string) string {
		v, err := lookupToken(tok[1:len(tok)-1], e, respBody)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return tok
		}
		return fmt.Sprint(v)
	})
	return out, firstErr
}

// A string leaf that is exactly one token keeps the source value's JSON type
// (an amount stays a number); anything else renders as text.
func renderBody(tmpl json.RawMessage, e Effect, respBody string) ([]byte, error) {
	if len(tmpl) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(tmpl, &v); err != nil {
		return nil, fmt.Errorf("inverse body template: %w", err)
	}
	rendered, err := renderValue(v, e, respBody)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rendered)
}

func renderValue(v any, e Effect, respBody string) (any, error) {
	switch t := v.(type) {
	case string:
		if m := token.FindString(t); m != "" && m == t {
			return lookupToken(t[1:len(t)-1], e, respBody)
		}
		return renderString(t, e, respBody)
	case map[string]any:
		for k, child := range t {
			r, err := renderValue(child, e, respBody)
			if err != nil {
				return nil, err
			}
			t[k] = r
		}
		return t, nil
	case []any:
		for i, child := range t {
			r, err := renderValue(child, e, respBody)
			if err != nil {
				return nil, err
			}
			t[i] = r
		}
		return t, nil
	default:
		return v, nil
	}
}
