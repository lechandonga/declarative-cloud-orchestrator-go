// Command orchestrator is a local, file-backed declarative cloud
// orchestrator: declarations and the simulated cloud are plain files.
//
// Usage:
//
//	orchestrator [-dir decls] [-state state.json] [-cloud cloud] [-wait] plan|apply|refresh|destroy
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/lechandonga/declarative-cloud-orchestrator-go/cloud"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/decl"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/engine"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/state"
)

func main() {
	dir := flag.String("dir", "decls", "declaration directory")
	statePath := flag.String("state", "state.json", "state file path")
	cloudDir := flag.String("cloud", "cloud", "simulated cloud directory")
	wait := flag.Bool("wait", false, "wait for the state lock instead of failing fast")
	flag.Parse()
	args := flag.Args()
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: orchestrator [flags] plan|apply|refresh|destroy")
		os.Exit(2)
	}
	cmd := args[0]

	set, err := decl.LoadDir(*dir)
	if err != nil {
		fatal(err)
	}
	store := state.NewStore(*statePath)
	if err := store.Lock(*wait); err != nil {
		if errors.Is(err, state.ErrLocked) {
			fatal(fmt.Errorf("another run holds the state lock; retry or pass -wait to queue"))
		}
		fatal(err)
	}
	defer store.Unlock()

	st, err := store.Load()
	if err != nil {
		fatal(err)
	}
	eng := &engine.Engine{Decls: set, Cloud: cloud.New(*cloudDir), Store: store}

	switch cmd {
	case "refresh":
		drift, err := eng.Refresh(st)
		if err != nil {
			fatal(err)
		}
		if err := store.Save(st); err != nil {
			fatal(err)
		}
		printDrift(drift)
	case "plan":
		plan, err := eng.Plan(st)
		if err != nil {
			fatal(err)
		}
		printDrift(plan.Drift)
		printPlan(plan)
	case "apply":
		plan, err := eng.Apply(st)
		if err != nil {
			store.Save(st) // persist checkpoints reached so far
			fatal(err)
		}
		if err := store.Save(st); err != nil {
			fatal(err)
		}
		printDrift(plan.Drift)
		printPlan(plan)
		if plan.Empty() {
			fmt.Println("No changes. Infrastructure is up to date.")
		} else {
			fmt.Println("Apply complete.")
		}
	case "destroy":
		// Plan against an empty declaration set is a full teardown; simpler:
		// delete everything recorded in state, in reverse order.
		for i := len(set.Order) - 1; i >= 0; i-- {
			addr := set.Order[i]
			rec := st.Resources[addr]
			if rec == nil {
				continue
			}
			if err := eng.Cloud.Delete(rec.Type, rec.Name); err != nil && !errors.Is(err, cloud.ErrNotFound) {
				store.Save(st)
				fatal(fmt.Errorf("destroy %s: %w", addr, err))
			}
			delete(st.Resources, addr)
			if err := store.Save(st); err != nil {
				fatal(err)
			}
			fmt.Printf("- %s destroyed\n", addr)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func printDrift(drift []engine.DriftEntry) {
	for _, d := range drift {
		fmt.Printf("! drift %s: %s outside of orchestrator\n", d.Addr, d.Kind)
		for _, df := range d.Diffs {
			fmt.Printf("    %s: %v -> %v\n", df.Path, df.Old, df.New)
		}
	}
}

func printPlan(p *engine.Plan) {
	for _, c := range p.Changes {
		switch c.Action {
		case engine.ActionNoop:
			fmt.Printf("  %s (no change)\n", c.Addr)
		default:
			fmt.Printf("%s %s\n", symbol(c.Action), c.Addr)
			for _, d := range c.Diffs {
				fmt.Printf("    %s: %v -> %v\n", d.Path, d.Old, d.New)
			}
		}
	}
}

func symbol(a engine.Action) string {
	switch a {
	case engine.ActionCreate:
		return "+"
	case engine.ActionUpdate:
		return "~"
	case engine.ActionDelete:
		return "-"
	}
	return "?"
}
