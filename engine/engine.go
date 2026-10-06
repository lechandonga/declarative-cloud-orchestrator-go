// Package engine ties declarations, the simulated cloud and the state store
// together: refresh (drift detection), plan and apply.
package engine

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/lechandonga/declarative-cloud-orchestrator-go/cloud"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/decl"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/state"
)

// Engine orchestrates one working directory.
type Engine struct {
	Decls *decl.Set
	Cloud *cloud.Client
	Store *state.Store
}

// Action is the kind of change planned for a resource.
type Action string

const (
	ActionNoop   Action = "noop"
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
)

// AttrDiff describes one differing attribute.
type AttrDiff struct {
	Path string
	Old  any `json:",omitempty"`
	New  any `json:",omitempty"`
}

// Change is one planned resource operation.
type Change struct {
	Addr   string
	Action Action
	Diffs  []AttrDiff // for update
}

// Plan is the full, deterministic list of changes.
type Plan struct {
	Changes []Change
	Drift   []DriftEntry // out-of-band changes detected during refresh
}

// Empty reports whether the plan contains no changes.
func (p *Plan) Empty() bool {
	for _, c := range p.Changes {
		if c.Action != ActionNoop {
			return false
		}
	}
	return true
}

// DriftEntry describes a difference between recorded state and the real
// (simulated) cloud discovered during refresh.
type DriftEntry struct {
	Addr  string
	Kind  string // "deleted", "modified"
	Diffs []AttrDiff
}

var attrRef = regexp.MustCompile(`^\$\{([A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\.([A-Za-z0-9_.-]+)\}$`)

// resolve substitutes ${type.name} and ${type.name.attr} references using
// already-known state properties. Whole-string attribute references are
// replaced by the raw value; others are string-interpolated with the
// referenced resource's address.
func resolve(props map[string]any, st *state.State) (map[string]any, error) {
	var walk func(v any) (any, error)
	walk = func(v any) (any, error) {
		switch t := v.(type) {
		case string:
			if m := attrRef.FindStringSubmatch(t); m != nil {
				r := st.Resources[m[1]]
				if r == nil {
					return nil, fmt.Errorf("cannot resolve %s: %s not yet in state", t, m[1])
				}
				cur := any(r.Properties)
				for _, part := range strings.Split(m[2], ".") {
					mm, ok := cur.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("cannot resolve %s: %q is not an object", t, part)
					}
					cur, ok = mm[part]
					if !ok {
						return nil, fmt.Errorf("cannot resolve %s: attribute %q missing", t, part)
					}
				}
				return cur, nil
			}
			return t, nil
		case map[string]any:
			out := map[string]any{}
			for k, vv := range t {
				rv, err := walk(vv)
				if err != nil {
					return nil, err
				}
				out[k] = rv
			}
			return out, nil
		case []any:
			out := make([]any, len(t))
			for i, vv := range t {
				rv, err := walk(vv)
				if err != nil {
					return nil, err
				}
				out[i] = rv
			}
			return out, nil
		default:
			return t, nil
		}
	}
	r, err := walk(props)
	if err != nil {
		return nil, err
	}
	return r.(map[string]any), nil
}

// Refresh reads every state-recorded resource from the cloud, detects drift,
// and updates the state to match reality. Callers must hold the store lock.
func (e *Engine) Refresh(st *state.State) ([]DriftEntry, error) {
	var drift []DriftEntry
	for addr, rec := range st.Resources {
		inst, err := e.Cloud.Read(rec.Type, rec.Name)
		if errors.Is(err, cloud.ErrNotFound) {
			drift = append(drift, DriftEntry{Addr: addr, Kind: "deleted"})
			delete(st.Resources, addr)
			continue
		}
		if err != nil {
			return nil, err
		}
		if diffs := diffProps("", rec.Properties, inst.Properties); len(diffs) > 0 {
			drift = append(drift, DriftEntry{Addr: addr, Kind: "modified", Diffs: diffs})
			rec.Properties = inst.Properties
		}
	}
	sort.Slice(drift, func(i, j int) bool { return drift[i].Addr < drift[j].Addr })
	return drift, nil
}

// Plan computes the changes needed to converge the cloud to the
// declarations. It refreshes first, so drift is reported and corrected
// rather than ignored. Callers must hold the store lock.
func (e *Engine) Plan(st *state.State) (*Plan, error) {
	drift, err := e.Refresh(st)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Drift: drift}

	// Deletes: managed resources no longer declared (reverse topo order).
	managed := map[string]bool{}
	for addr := range e.Decls.Resources {
		managed[addr] = true
	}
	var deletes []string
	for addr := range st.Resources {
		if !managed[addr] {
			deletes = append(deletes, addr)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(deletes)))

	// Creates/updates in topological order, resolving references against a
	// working copy of state that includes resources planned for creation.
	working := &state.State{Resources: map[string]*state.Resource{}}
	for k, v := range st.Resources {
		working.Resources[k] = v
	}
	for _, addr := range e.Decls.Order {
		r := e.Decls.Resources[addr]
		props, err := resolve(r.Properties, working)
		if err != nil {
			return nil, err
		}
		cur, ok := st.Resources[addr]
		switch {
		case !ok:
			plan.Changes = append(plan.Changes, Change{Addr: addr, Action: ActionCreate})
		case !reflect.DeepEqual(cur.Properties, props):
			plan.Changes = append(plan.Changes, Change{
				Addr: addr, Action: ActionUpdate,
				Diffs: diffProps("", cur.Properties, props),
			})
		default:
			plan.Changes = append(plan.Changes, Change{Addr: addr, Action: ActionNoop})
		}
		working.Resources[addr] = &state.Resource{Type: r.Type, Name: r.Name, Properties: props}
	}
	for _, addr := range deletes {
		plan.Changes = append(plan.Changes, Change{Addr: addr, Action: ActionDelete})
	}
	return plan, nil
}

// Apply recomputes the plan under the lock and executes it in order,
// checkpointing state after every resource so an interrupted run resumes
// where it left off instead of duplicating or skipping resources.
func (e *Engine) Apply(st *state.State) (*Plan, error) {
	plan, err := e.Plan(st)
	if err != nil {
		return nil, err
	}
	for _, ch := range plan.Changes {
		switch ch.Action {
		case ActionNoop:
			continue
		case ActionCreate, ActionUpdate:
			r := e.Decls.Resources[ch.Addr]
			props, err := resolve(r.Properties, st)
			if err != nil {
				return plan, err
			}
			inst := cloud.Instance{Type: r.Type, Name: r.Name, Properties: props}
			var opErr error
			if ch.Action == ActionCreate {
				opErr = e.Cloud.Create(inst)
			} else {
				opErr = e.Cloud.Update(inst)
			}
			if opErr != nil {
				return plan, fmt.Errorf("apply %s (%s): %w", ch.Addr, ch.Action, opErr)
			}
			st.Resources[ch.Addr] = &state.Resource{Type: r.Type, Name: r.Name, Properties: props}
		case ActionDelete:
			rec := st.Resources[ch.Addr]
			if err := e.Cloud.Delete(rec.Type, rec.Name); err != nil && !errors.Is(err, cloud.ErrNotFound) {
				return plan, fmt.Errorf("apply %s (delete): %w", ch.Addr, err)
			}
			delete(st.Resources, ch.Addr)
		}
		// Checkpoint after each resource: crash-safe resume.
		if err := e.Store.Save(st); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

// diffProps recursively compares old and new property maps.
func diffProps(prefix string, old, new map[string]any) []AttrDiff {
	var out []AttrDiff
	keys := map[string]bool{}
	for k := range old {
		keys[k] = true
	}
	for k := range new {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		ov, hasOld := old[k]
		nv, hasNew := new[k]
		om, ook := ov.(map[string]any)
		nm, nok := nv.(map[string]any)
		if ook && nok {
			out = append(out, diffProps(path, om, nm)...)
			continue
		}
		if !hasOld || !hasNew || !reflect.DeepEqual(ov, nv) {
			out = append(out, AttrDiff{Path: path, Old: ov, New: nv})
		}
	}
	return out
}
