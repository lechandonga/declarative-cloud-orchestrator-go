package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lechandonga/declarative-cloud-orchestrator-go/cloud"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/decl"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/state"
)

type fixture struct {
	eng   *Engine
	cloud *cloud.Client
	store *state.Store
	root  string
}

// newFixture builds a working directory with a two-tier declaration.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	declDir := filepath.Join(root, "decls")
	os.Mkdir(declDir, 0o755)
	os.WriteFile(filepath.Join(declDir, "net.json"), []byte(`{"resources":[
		{"type":"net","name":"vpc","properties":{"cidr":"10.0.0.0/16"}}]}`), 0o644)
	os.WriteFile(filepath.Join(declDir, "app.json"), []byte(`{"resources":[
		{"type":"db","name":"main","properties":{"cidr":"${net.vpc.cidr}","size":2}},
		{"type":"app","name":"web","depends_on":["db.main"],"properties":{"image":"web:1"}}]}`), 0o644)
	set, err := decl.LoadDir(declDir)
	if err != nil {
		t.Fatal(err)
	}
	c := cloud.New(filepath.Join(root, "cloud"))
	store := state.NewStore(filepath.Join(root, "state.json"))
	return &fixture{eng: &Engine{Decls: set, Cloud: c, Store: store}, cloud: c, store: store, root: root}
}

func (f *fixture) lockedState(t *testing.T) *state.State {
	t.Helper()
	if err := f.store.Lock(false); err != nil {
		t.Fatal(err)
	}
	st, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (f *fixture) apply(t *testing.T) *Plan {
	t.Helper()
	st := f.lockedState(t)
	defer f.store.Unlock()
	p, err := f.eng.Apply(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Save(st); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanMatchesApplyAndIdempotent(t *testing.T) {
	t.Log("业务场景: 计划/执行一致性与幂等——plan 展示的变更与 apply 实际行为一致，无变化时重复 apply 不触碰资源")
	f := newFixture(t)

	st := f.lockedState(t)
	plan, err := f.eng.Plan(st)
	if err != nil {
		t.Fatal(err)
	}
	f.store.Unlock()
	var creates []string
	for _, c := range plan.Changes {
		if c.Action != ActionCreate {
			t.Fatalf("first plan should be all creates, got %v", c)
		}
		creates = append(creates, c.Addr)
	}
	t.Logf("用例: 首次 plan 全部为 create: %v", creates)

	applied := f.apply(t)
	if len(applied.Changes) != len(plan.Changes) {
		t.Fatalf("apply changed a different number of resources than planned")
	}
	if !reflect.DeepEqual(applied.Changes, plan.Changes) {
		t.Fatalf("plan/apply mismatch: %v vs %v", plan.Changes, applied.Changes)
	}
	t.Log("用例通过: apply 实际执行与 plan 完全一致")

	// Record cloud files, re-apply, ensure nothing is rewritten.
	cloudDir := f.cloudDir(t)
	before := snapshot(t, cloudDir)
	p2 := f.apply(t)
	if !p2.Empty() {
		t.Fatalf("second apply should be empty, got %+v", p2.Changes)
	}
	after := snapshot(t, cloudDir)
	for k, v := range after {
		if before[k] != v {
			t.Fatalf("idempotent apply modified cloud object %s", k)
		}
	}
	t.Log("用例通过: 无变化时重复 apply 未改动任何云端对象")

	// Attribute reference resolved from dependency.
	inst, err := f.cloud.Read("db", "main")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Properties["cidr"] != "10.0.0.0/16" {
		t.Fatalf("reference not resolved: %v", inst.Properties)
	}
	t.Log("用例通过: ${net.vpc.cidr} 引用在 apply 时被正确解析")
}

func (f *fixture) cloudDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(f.root, "cloud")
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		out[e.Name()] = string(b)
	}
	return out
}

func TestFailureRetryResumesWithoutDuplication(t *testing.T) {
	t.Log("业务场景: 失败重试——执行中途失败后重试只继续未完成部分，不重复创建、不漏资源")
	f := newFixture(t)
	f.cloud.InjectFailure("create", "db.main", errors.New("simulated provider crash"))

	st := f.lockedState(t)
	_, err := f.eng.Apply(st)
	if err == nil {
		t.Fatal("expected apply to fail")
	}
	f.store.Save(st)
	f.store.Unlock()
	t.Logf("用例: 首次 apply 在 db.main 处注入失败: %v", err)

	// net.vpc must have been created and checkpointed; db.main must not exist.
	if _, err := f.cloud.Read("net", "vpc"); err != nil {
		t.Fatal("net.vpc should have been created before the failure")
	}
	if _, err := f.cloud.Read("db", "main"); !errors.Is(err, cloud.ErrNotFound) {
		t.Fatal("db.main should not exist after failed create")
	}
	t.Log("用例通过: 失败前已完成的资源被检查点保存，失败资源未创建")

	// Retry: failure was one-shot, so it now succeeds and completes the rest.
	p := f.apply(t)
	var actions []string
	for _, c := range p.Changes {
		if c.Action != ActionNoop {
			actions = append(actions, fmt.Sprintf("%s:%s", c.Action, c.Addr))
		}
	}
	want := []string{"create:db.main", "create:app.web"}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("retry should only do remaining work %v, did %v", want, actions)
	}
	for _, r := range [][2]string{{"net", "vpc"}, {"db", "main"}, {"app", "web"}} {
		if _, err := f.cloud.Read(r[0], r[1]); err != nil {
			t.Fatalf("missing resource after retry: %v", err)
		}
	}
	t.Logf("用例通过: 重试仅执行剩余变更 %v，无重复创建、无遗漏", actions)
}

func TestConcurrentApplySerialized(t *testing.T) {
	t.Log("业务场景: 并发执行——同一工作目录多个 apply 互斥，状态不交错")
	f := newFixture(t)

	// First runner holds the lock; a second non-blocking runner must fail.
	st := f.lockedState(t)
	store2 := state.NewStore(filepath.Join(f.root, "state.json"))
	if err := store2.Lock(false); !errors.Is(err, state.ErrLocked) {
		t.Fatalf("expected ErrLocked for concurrent runner, got %v", err)
	}
	t.Log("用例通过: 锁被占用时第二个执行者明确失败")

	// A waiting runner blocks until the first finishes, then proceeds.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s2 := state.NewStore(filepath.Join(f.root, "state.json"))
		if err := s2.Lock(true); err != nil { // orderly wait
			t.Error(err)
			return
		}
		defer s2.Unlock()
		st2, err := s2.Load()
		if err != nil {
			t.Error(err)
			return
		}
		if len(st2.Resources) != 3 {
			t.Errorf("waiting runner should observe complete state, got %d resources", len(st2.Resources))
		}
	}()
	time.Sleep(50 * time.Millisecond) // ensure the waiter is blocked
	if _, err := f.eng.Apply(st); err != nil {
		t.Fatal(err)
	}
	f.store.Save(st)
	f.store.Unlock()
	wg.Wait()
	t.Log("用例通过: 等待中的执行者有序获取锁并看到完整状态，无交错改写")
}

func TestDriftDetectionAndCorrection(t *testing.T) {
	t.Log("业务场景: 漂移——外部改动云端资源后，refresh/plan 必须识别差异并计划纠正")
	f := newFixture(t)
	f.apply(t)

	// External actor modifies and deletes cloud resources directly.
	inst, _ := f.cloud.Read("db", "main")
	inst.Properties["size"] = float64(99)
	if err := f.cloud.Update(inst); err != nil {
		t.Fatal(err)
	}
	if err := f.cloud.Delete("app", "web"); err != nil {
		t.Fatal(err)
	}

	st := f.lockedState(t)
	plan, err := f.eng.Plan(st)
	if err != nil {
		t.Fatal(err)
	}
	f.store.Save(st)
	f.store.Unlock()

	var kinds []string
	for _, d := range plan.Drift {
		kinds = append(kinds, d.Addr+":"+d.Kind)
	}
	if len(plan.Drift) != 2 {
		t.Fatalf("expected 2 drift entries, got %v", kinds)
	}
	t.Logf("用例通过: refresh 识别漂移 %v", kinds)

	var updateDiffs []AttrDiff
	var recreate bool
	for _, c := range plan.Changes {
		if c.Addr == "db.main" && c.Action == ActionUpdate {
			updateDiffs = c.Diffs
		}
		if c.Addr == "app.web" && c.Action == ActionCreate {
			recreate = true
		}
	}
	if len(updateDiffs) == 0 || !recreate {
		t.Fatalf("plan should update db.main and recreate app.web, got %+v", plan.Changes)
	}
	t.Logf("用例通过: plan 计划回改 db.main(size 99->2) 并重建被删的 app.web, diffs=%+v", updateDiffs)

	p := f.apply(t)
	if p.Empty() {
		t.Fatal("apply after drift should not be empty")
	}
	after, _ := f.cloud.Read("db", "main")
	if after.Properties["size"] != float64(2) {
		t.Fatalf("drift not corrected: %v", after.Properties)
	}
	if _, err := f.cloud.Read("app", "web"); err != nil {
		t.Fatal("deleted resource not recreated")
	}
	t.Log("用例通过: apply 将漂移纠正回声明状态")
}
