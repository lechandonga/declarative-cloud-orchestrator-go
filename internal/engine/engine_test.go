package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lechandonga/declarative-cloud-orchestrator-go/internal/cloud"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/internal/manifest"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/internal/state"
)

const validManifest = `
module: net
resources:
  - name: vpc
    kind: network
    attrs:
      cidr: 10.0.0.0/16
  - name: subnet
    kind: subnet
    attrs:
      cidr: 10.0.1.0/24
    depends_on:
      - net.vpc
---
module: app
resources:
  - name: web
    kind: function
    attrs:
      vpc_cidr: ${net.vpc.cidr}
      name: web
    depends_on:
      - net.subnet
`

func setupManifest(t *testing.T, dir, docs string) {
	t.Helper()
	pieces := strings.SplitN(strings.TrimSpace(docs), "---", 2)
	names := []string{"net.yaml", "app.yaml"}
	for i, p := range pieces {
		if err := os.WriteFile(filepath.Join(dir, names[i]), []byte(strings.TrimSpace(p)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type fixture struct {
	man   *manifest.Manifest
	bk    *cloud.Backend
	store *state.Store
	eng   *Engine
	dir   string
}

func newFixtureLocked(t *testing.T, docs string) *fixture {
	t.Helper()
	root := t.TempDir()
	manDir := filepath.Join(root, "manifests")
	workDir := filepath.Join(root, "work")
	cloudDir := filepath.Join(root, "cloud")
	if err := os.MkdirAll(manDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setupManifest(t, manDir, docs)
	man, err := manifest.LoadDir(manDir)
	if err != nil {
		t.Fatalf("声明加载失败: %v", err)
	}
	bk, err := cloud.NewBackend(cloudDir)
	if err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(workDir)
	if _, err := store.TryLock(workDir); err != nil {
		t.Fatal(err)
	}
	return &fixture{man: man, bk: bk, store: store, eng: New(man, bk, store), dir: root}
}

func (f *fixture) unlock() { _ = f.store.Unlock() }

func cloudAttrs(t *testing.T, bk *cloud.Backend, id string) map[string]string {
	t.Helper()
	r, err := bk.Read(id)
	if err != nil {
		t.Fatalf("读取云端资源 %s: %v", id, err)
	}
	return r.Attrs
}

func appliedIDs(r *ApplyResult) []string {
	var ids []string
	for _, a := range r.Applied {
		ids = append(ids, a.ResourceID)
	}
	return ids
}

// 业务：计划幂等 —— plan 展示的变更与 apply 实际动作一致；
// 无变化时重复 plan/apply 不会再触碰任何资源。
func TestPlanApplyIdempotency(t *testing.T) {
	t.Log("业务[计划幂等]: plan 与 apply 一致、重复 apply 零改动")
	f := newFixtureLocked(t, validManifest)
	defer f.unlock()

	p, err := f.eng.Plan()
	if err != nil {
		t.Fatalf("plan 失败: %v", err)
	}
	if p.ChangedCount() != 3 {
		t.Fatalf("首次计划应有 3 个创建动作，实际 %d: %+v", p.ChangedCount(), p.Actions)
	}
	planned := map[string]Op{}
	for _, a := range p.Actions {
		planned[a.ResourceID] = a.Op
	}
	if planned["net.vpc"] != OpCreate || planned["app.web"] != OpCreate {
		t.Fatalf("计划动作不符合预期: %+v", planned)
	}
	t.Logf("用例[plan 展示 3 个创建]: 通过，指纹=%s", p.Fingerprint[:12])

	res, err := f.eng.Apply()
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	if len(res.Applied) != 3 {
		t.Fatalf("apply 应实际改动 3 个资源，实际 %d", len(res.Applied))
	}
	if got := cloudAttrs(t, f.bk, "app.web")["vpc_cidr"]; got != "10.0.0.0/16" {
		t.Fatalf("引用属性未落地: %q", got)
	}
	t.Log("用例[apply 与 plan 一致，引用属性落地]: 通过")

	p2, err := f.eng.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if p2.ChangedCount() != 0 {
		t.Fatalf("收敛后计划变更数应为 0，实际 %d", p2.ChangedCount())
	}
	res2, err := f.eng.Apply()
	if err != nil {
		t.Fatalf("无计划变更时 apply 应幂等成功: %v", err)
	}
	if len(res2.Applied) != 0 || len(res2.Skipped) != 3 {
		t.Fatalf("重复 apply 不应改动任何资源: applied=%d skipped=%d", len(res2.Applied), len(res2.Skipped))
	}
	t.Log("业务[计划幂等]: 用例[重复 apply 零改动，全部跳过] 通过")
}

// 业务：plan/apply 一致性 —— plan 之后云端被改动，apply 必须拒绝并要求重新 plan。
func TestPlanApplyFingerprintMismatch(t *testing.T) {
	t.Log("业务[计划一致]: plan 后云端变化，apply 必须拒绝而非按旧计划执行")
	f := newFixtureLocked(t, validManifest)
	defer f.unlock()

	if _, err := f.eng.Plan(); err != nil {
		t.Fatal(err)
	}
	if err := f.bk.Create(cloud.Resource{
		ID: "net.vpc", Kind: "network", Attrs: map[string]string{"cidr": "192.168.0.0/16"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.Apply(); err == nil || !strings.Contains(err.Error(), "计划已过期") {
		t.Fatalf("期望计划过期错误，得到 %v", err)
	}
	t.Log("用例[指纹不一致拒绝 apply]: 通过")
}

// 业务：失败重试 —— 中途注入故障后，重试只能继续未完成资源，
// 已创建的资源绝不重复创建，也不漏掉任何资源。
func TestFailureRetryContinuesOnlyRemaining(t *testing.T) {
	t.Log("业务[失败重试]: 注入故障 -> 中断 -> 重新 plan/apply 只补剩余资源")
	f := newFixtureLocked(t, validManifest)
	defer f.unlock()

	cloudDir := filepath.Join(f.dir, "cloud")
	if err := cloud.InjectFault(cloudDir, "net.subnet", "create", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.Plan(); err != nil {
		t.Fatal(err)
	}
	res, err := f.eng.Apply()
	if err == nil {
		t.Fatal("期望 apply 因注入故障失败")
	}
	if res.FailedOn != "net.subnet" || len(res.Applied) != 1 || res.Applied[0].ResourceID != "net.vpc" {
		t.Fatalf("中断位置/检查点不符: failed=%s applied=%v", res.FailedOn, appliedIDs(res))
	}
	t.Log("用例[首次 apply 在 net.subnet 中断，仅完成 net.vpc]: 通过")

	if _, err := f.bk.Read("net.subnet"); err == nil {
		t.Fatal("失败的资源不应在云端留下记录")
	}

	p2, err := f.eng.Plan()
	if err != nil {
		t.Fatal(err)
	}
	todo := map[string]bool{}
	for _, a := range p2.Actions {
		if a.Op != OpNoop {
			todo[a.ResourceID] = true
		}
	}
	if len(todo) != 2 || !todo["net.subnet"] || !todo["app.web"] {
		t.Fatalf("重试计划应只剩 subnet+web，实际 %+v", todo)
	}
	if p2.Actions[0].Op != OpNoop || p2.Actions[0].ResourceID != "net.vpc" {
		t.Fatal("已完成的 net.vpc 在重试计划中必须是无变更，而不是重复创建")
	}
	t.Log("用例[重试计划不重复创建已完成资源]: 通过")

	res2, err := f.eng.Apply()
	if err != nil {
		t.Fatalf("清除故障后重试应成功: %v", err)
	}
	if len(res2.Applied) != 2 {
		t.Fatalf("重试应只补建 2 个资源，实际 %d: %v", len(res2.Applied), appliedIDs(res2))
	}

	ids, err := f.bk.List()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "app.web,net.subnet,net.vpc" {
		t.Fatalf("最终云端资源不全或有重复: %v", ids)
	}
	t.Log("业务[失败重试]: 用例[重试恰好补齐 2 个资源，最终无重无漏] 通过")
}

// 业务：漂移识别 —— 外部改属性、外部删除、云端多余资源，refresh/plan 都要说清差异。
func TestDriftDetection(t *testing.T) {
	t.Log("业务[漂移]: 外部改属性、外部删除、云端多余资源三类漂移")
	f := newFixtureLocked(t, validManifest)
	defer f.unlock()

	if _, err := f.eng.Plan(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.Apply(); err != nil {
		t.Fatal(err)
	}

	overwriteCloudFile(t, f.dir, "net.vpc", cloud.Resource{
		ID: "net.vpc", Kind: "network", Attrs: map[string]string{"cidr": "172.16.0.0/16"},
	})
	if err := os.Remove(filepath.Join(f.dir, "cloud", "resources", "net.subnet.json")); err != nil {
		t.Fatal(err)
	}
	overwriteCloudFile(t, f.dir, "ghost.ext", cloud.Resource{
		ID: "ghost.ext", Kind: "bucket", Attrs: map[string]string{},
	})

	drifts, err := f.eng.Refresh()
	if err != nil {
		t.Fatalf("refresh 失败: %v", err)
	}
	byID := map[string]Drift{}
	for _, d := range drifts {
		byID[d.ResourceID] = d
	}
	d, ok := byID["net.vpc"]
	if !ok || d.Reason != "attributes-changed" || len(d.Diffs) != 1 || d.Diffs[0].Actual != "172.16.0.0/16" {
		t.Fatalf("属性漂移未正确识别: %+v", d)
	}
	if d, ok := byID["net.subnet"]; !ok || d.Reason != "deleted-externally" {
		t.Fatalf("外部删除漂移未识别: %+v", byID)
	}
	if d, ok := byID["ghost.ext"]; !ok || d.Reason != "unexpected-extra" {
		t.Fatalf("多余资源漂移未识别: %+v", byID)
	}
	t.Logf("用例[refresh 识别三类漂移并给出属性差异]: 通过（%d 处）", len(drifts))

	p, err := f.eng.Plan()
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]Op{}
	for _, a := range p.Actions {
		ops[a.ResourceID] = a.Op
	}
	if ops["net.vpc"] != OpUpdate || ops["net.subnet"] != OpRecreate || p.ChangedCount() != 2 {
		t.Fatalf("漂移后的计划动作错误: %+v", ops)
	}
	if _, err := f.eng.Apply(); err != nil {
		t.Fatalf("按漂移计划收敛失败: %v", err)
	}
	if got := cloudAttrs(t, f.bk, "net.vpc")["cidr"]; got != "10.0.0.0/16" {
		t.Fatalf("漂移未被修正: %q", got)
	}
	if _, err := f.bk.Read("net.subnet"); err != nil {
		t.Fatalf("被外部删除的资源应被重建: %v", err)
	}
	if _, err := f.bk.Read("ghost.ext"); err != nil {
		t.Fatalf("多余资源不应被自动删除: %v", err)
	}
	t.Log("业务[漂移]: 用例[plan 识别漂移并按 update/重建收敛，多余资源只告警不删除] 通过")
}

func overwriteCloudFile(t *testing.T, root, id string, r cloud.Resource) {
	t.Helper()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cloud", "resources", id+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildEnv(t *testing.T, root string) (*manifest.Manifest, *cloud.Backend) {
	t.Helper()
	manDir := filepath.Join(root, "manifests")
	if err := os.MkdirAll(manDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setupManifest(t, manDir, validManifest)
	man, err := manifest.LoadDir(manDir)
	if err != nil {
		t.Fatal(err)
	}
	bk, err := cloud.NewBackend(filepath.Join(root, "cloud"))
	if err != nil {
		t.Fatal(err)
	}
	return man, bk
}

// 业务：并发执行 —— 多个执行同时启动时，抢不到锁的一方要么立即明确失败，
// 要么有序等待；任何情况下状态文件都不被交错改写。
func TestConcurrentApply_LockPolicies(t *testing.T) {
	t.Log("业务[并发执行]: 两个执行同时启动，锁策略必须是明确失败或有序等待")

	t.Run("非阻塞抢锁失败立即报错", func(t *testing.T) {
		root := t.TempDir()
		workDir := filepath.Join(root, "work")
		holder := state.NewStore(workDir)
		ok, err := holder.TryLock(workDir)
		if !ok || err != nil {
			t.Fatalf("第一执行方应拿到锁: ok=%v err=%v", ok, err)
		}
		defer holder.Unlock()

		other := state.NewStore(workDir)
		ok, err = other.TryLock(workDir)
		if ok || err != nil {
			t.Fatalf("第二执行方抢锁应失败且无错误，得到 ok=%v err=%v", ok, err)
		}
		t.Log("用例[锁被占用时非阻塞方立即得到 false]: 通过")
	})

	t.Run("等待方有序续跑且状态无交错", func(t *testing.T) {
		root := t.TempDir()
		workDir := filepath.Join(root, "work")
		man, bk := buildEnv(t, root)

		run := func(wg *sync.WaitGroup, started, proceed chan struct{}, hold bool, result *error) {
			defer wg.Done()
			store := state.NewStore(workDir)
			close(started)
			if proceed != nil {
				<-proceed
			}
			if err := store.Lock(workDir); err != nil {
				*result = err
				return
			}
			defer store.Unlock()
			if hold {
				// 持锁期间模拟一轮完整编排，结束才释放，第二方应在此期间排队。
			}
			eng := New(man, bk, store)
			if _, err := eng.Plan(); err != nil {
				*result = err
				return
			}
			if _, err := eng.Apply(); err != nil {
				*result = err
				return
			}
		}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		s1 := make(chan struct{})
		s2 := make(chan struct{})
		proceed2 := make(chan struct{})

		// 第二方先启动并在取锁前排队等待信号，保证它在第一方持锁期间发起 Lock。
		wg.Add(1)
		go run(&wg, s2, proceed2, false, &errs[1])
		<-s1
		time.Sleep(20 * time.Millisecond)

		wg.Add(1)
		go run(&wg, s1, nil, true, &errs[0])
		<-s1
		// 第一方已持锁，此时放行第二方，它应阻塞在 Lock 上直到第一方完成。
		close(proceed2)
		wg.Wait()

		for i, e := range errs {
			if e != nil {
				t.Fatalf("第 %d 个并发执行失败: %v", i+1, e)
			}
		}

		// 串行化的最终结果：资源恰好 3 个，状态文件 JSON 合法且只有 3 条记录。
		ids, err := bk.List()
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(ids)
		if strings.Join(ids, ",") != "app.web,net.subnet,net.vpc" {
			t.Fatalf("并发后云端资源被交错改写或重复: %v", ids)
		}
		final, err := state.NewStore(workDir).Load()
		if err != nil {
			t.Fatalf("最终状态不可读，说明被交错写坏: %v", err)
		}
		if len(final.Resources) != 3 {
			t.Fatalf("最终状态应含 3 个资源，实际 %d", len(final.Resources))
		}
		t.Log("业务[并发执行]: 用例[两个并发执行有序串行，状态与云端均无交错、无重复] 通过")
	})
}
