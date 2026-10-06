// Package decl loads resource declarations from a directory of JSON files,
// validates them (duplicates, dangling references, dependency cycles) and
// produces a deterministic topological execution order.
package decl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Resource is a single declared cloud resource.
type Resource struct {
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Properties map[string]any `json:"properties"`
	DependsOn  []string       `json:"depends_on"`
	File       string         `json:"-"` // source file, for diagnostics
}

// Addr returns the canonical address "type.name".
func (r Resource) Addr() string { return r.Type + "." + r.Name }

// ErrorKind distinguishes validation failure categories so callers and
// users can tell exactly why a declaration set was rejected.
type ErrorKind int

const (
	KindDuplicate ErrorKind = iota // same address defined twice
	KindDangling                   // reference/depends_on to undefined resource
	KindCycle                      // dependency cycle
	KindParse                      // malformed declaration file
)

// Error is a validation error with a machine-checkable kind.
type Error struct {
	Kind ErrorKind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Set is a validated set of declarations.
type Set struct {
	Resources map[string]*Resource // by address
	Order     []string             // addresses in topological order
}

var refPattern = regexp.MustCompile(`\$\{([A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)(\.[A-Za-z0-9_.-]+)?\}`)

// References returns the resource addresses referenced from the resource's
// property values via ${type.name} or ${type.name.attr} expressions.
func References(props map[string]any) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			for _, m := range refPattern.FindAllStringSubmatch(t, -1) {
				out = append(out, m[1])
			}
		case map[string]any:
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(props)
	return out
}

// fileFormat is the on-disk shape of a declaration file.
type fileFormat struct {
	Resources []Resource `json:"resources"`
}

// LoadDir reads every *.json declaration file in dir, merges them, validates
// and returns a Set. Any validation problem aborts with a *Error whose Kind
// identifies the failure category; nothing is silently skipped.
func LoadDir(dir string) (*Set, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read declaration dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, &Error{Kind: KindParse, Msg: fmt.Sprintf("no declaration files (*.json) in %s", dir)}
	}

	resources := map[string]*Resource{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		var ff fileFormat
		if err := json.Unmarshal(data, &ff); err != nil {
			return nil, &Error{Kind: KindParse, Msg: fmt.Sprintf("parse %s: %v", f, err)}
		}
		for i := range ff.Resources {
			r := ff.Resources[i]
			r.File = f
			if r.Type == "" || r.Name == "" {
				return nil, &Error{Kind: KindParse, Msg: fmt.Sprintf("%s: resource with empty type or name", f)}
			}
			addr := r.Addr()
			if prev, ok := resources[addr]; ok {
				return nil, &Error{Kind: KindDuplicate, Msg: fmt.Sprintf(
					"duplicate resource %q: defined in %s and %s", addr, prev.File, f)}
			}
			rc := r
			resources[addr] = &rc
		}
	}

	// Build dependency graph: explicit depends_on plus implicit references.
	deps := map[string]map[string]bool{}
	for addr, r := range resources {
		deps[addr] = map[string]bool{}
		for _, d := range r.DependsOn {
			if _, ok := resources[d]; !ok {
				return nil, &Error{Kind: KindDangling, Msg: fmt.Sprintf(
					"%s (in %s) depends_on %q which is not defined", addr, r.File, d)}
			}
			deps[addr][d] = true
		}
		for _, ref := range References(r.Properties) {
			if _, ok := resources[ref]; !ok {
				return nil, &Error{Kind: KindDangling, Msg: fmt.Sprintf(
					"%s (in %s) references %q which is not defined", addr, r.File, ref)}
			}
			if ref != addr {
				deps[addr][ref] = true
			}
		}
	}

	order, err := topoSort(deps)
	if err != nil {
		return nil, err
	}
	return &Set{Resources: resources, Order: order}, nil
}

// topoSort returns addresses in dependency-first order (Kahn's algorithm,
// alphabetical tie-break for determinism) or a KindCycle error naming the
// resources involved in a cycle.
func topoSort(deps map[string]map[string]bool) ([]string, error) {
	indegree := map[string]int{}
	dependents := map[string][]string{}
	for addr, ds := range deps {
		indegree[addr] = len(ds)
		for d := range ds {
			dependents[d] = append(dependents[d], addr)
		}
	}
	var ready []string
	for addr, n := range indegree {
		if n == 0 {
			ready = append(ready, addr)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		addr := ready[0]
		ready = ready[1:]
		order = append(order, addr)
		for _, dep := range dependents[addr] {
			indegree[dep]--
			if indegree[dep] == 0 {
				ready = append(ready, dep)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(deps) {
		var cyclic []string
		for addr, n := range indegree {
			if n > 0 {
				cyclic = append(cyclic, addr)
			}
		}
		sort.Strings(cyclic)
		return nil, &Error{Kind: KindCycle, Msg: fmt.Sprintf(
			"dependency cycle detected among: %s", strings.Join(cyclic, ", "))}
	}
	return order, nil
}
