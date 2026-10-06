package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/lechandonga/declarative-cloud-orchestrator-go/internal/cloud"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/internal/engine"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/internal/manifest"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/internal/state"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	manifestDir := fs.String("manifest", "manifests", "声明文件目录")
	workDir := fs.String("workdir", ".orchestrator", "状态工作目录")
	cloudDir := fs.String("clouddir", "", "模拟云目录（默认 <workdir>/cloud）")
	wait := fs.Bool("wait", false, "其他执行持锁时有序等待；默认抢锁失败立即报错")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *cloudDir == "" {
		*cloudDir = filepathJoin(*workDir, "cloud")
	}

	man, err := manifest.LoadDir(*manifestDir)
	if err != nil {
		fail("声明校验", err)
	}
	bk, err := cloud.NewBackend(*cloudDir)
	if err != nil {
		fail("初始化模拟云", err)
	}
	st := state.NewStore(*workDir)

	switch cmd {
	case "plan":
		runLocked(st, *workDir, *wait, func() error {
			eng := engine.New(man, bk, st)
			p, err := eng.Plan()
			if err != nil {
				return err
			}
			printPlan(p)
			return nil
		})
	case "apply":
		runLocked(st, *workDir, *wait, func() error {
			eng := engine.New(man, bk, st)
			res, err := eng.Apply()
			if err != nil {
				printApplyPartial(res)
				return err
			}
			printApply(res)
			return nil
		})
	case "refresh":
		runLocked(st, *workDir, *wait, func() error {
			eng := engine.New(man, bk, st)
			drifts, err := eng.Refresh()
			if err != nil {
				return err
			}
			printDrifts(drifts)
			return nil
		})
	case "inject-fault":
		// orchestrator inject-fault <resource> <operation> <times>
		pos := fs.Args()
		if len(pos) != 3 {
			fail("参数", fmt.Errorf("用法: inject-fault <resource> <create|update> <times>"))
		}
		if err := cloud.InjectFault(*cloudDir, pos[0], pos[1], atoi(pos[2])); err != nil {
			fail("注入故障", err)
		}
		fmt.Printf("已注入故障: resource=%s op=%s times=%s\n", pos[0], pos[1], pos[2])
	default:
		usage()
		os.Exit(2)
	}
}

func runLocked(st *state.Store, workDir string, wait bool, fn func() error) {
	if wait {
		if err := st.Lock(workDir); err != nil {
			fail("获取状态锁", err)
		}
	} else {
		ok, err := st.TryLock(workDir)
		if err != nil {
			fail("获取状态锁", err)
		}
		if !ok {
			fail("并发冲突", fmt.Errorf("另一个执行正在该工作目录上运行（锁被占用）；加 --wait 可有序等待，或稍后重试"))
		}
	}
	defer st.Unlock()
	if err := fn(); err != nil {
		fail("执行", err)
	}
}

func fail(stage string, err error) {
	fmt.Fprintf(os.Stderr, "[错误] %s: %v\n", stage, err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, strings.TrimSpace(`
声明式云资源编排引擎（本地模拟）

用法:
  orchestrator plan     [-manifest DIR] [-workdir DIR] [-clouddir DIR] [--wait]
  orchestrator apply    [-manifest DIR] [-workdir DIR] [-clouddir DIR] [--wait]
  orchestrator refresh  [-manifest DIR] [-workdir DIR] [-clouddir DIR] [--wait]
  orchestrator inject-fault <resource> <create|update> <times> [同上参数]
`))
}

func printPlan(p *engine.Plan) {
	fmt.Println("=== Plan ===")
	for _, a := range p.Actions {
		switch a.Op {
		case engine.OpNoop:
			fmt.Printf("  无变更  %s (%s)\n", a.ResourceID, a.Kind)
		case engine.OpCreate:
			fmt.Printf("  + 创建  %s (%s) attrs=%s\n", a.ResourceID, a.Kind, attrsString(a.Desired))
		case engine.OpRecreate:
			fmt.Printf("  ± 重建  %s (%s) attrs=%s（云端资源被外部删除）\n", a.ResourceID, a.Kind, attrsString(a.Desired))
		case engine.OpUpdate:
			fmt.Printf("  ~ 更新  %s (%s)\n", a.ResourceID, a.Kind)
			for _, d := range a.Diffs {
				if !d.ActualExist {
					fmt.Printf("        + 属性 %s: %q（云端缺失）\n", d.Attr, d.Desired)
				} else {
					fmt.Printf("        ~ 属性 %s: %q -> %q\n", d.Attr, d.Actual, d.Desired)
				}
			}
		}
	}
	printDrifts(p.Drifts)
	fmt.Printf("计划变更资源数: %d\n", p.ChangedCount())
	fmt.Printf("计划指纹: %s\n", p.Fingerprint[:12])
}

func printApply(res *engine.ApplyResult) {
	fmt.Println("=== Apply ===")
	for _, a := range res.Applied {
		fmt.Printf("  已%s %s\n", opCN(a.Op), a.ResourceID)
	}
	for _, a := range res.Skipped {
		fmt.Printf("  跳过(无变更) %s\n", a.ResourceID)
	}
	fmt.Printf("本次改动 %d 个资源，跳过 %d 个资源\n", len(res.Applied), len(res.Skipped))
}

func printApplyPartial(res *engine.ApplyResult) {
	if res == nil {
		return
	}
	fmt.Println("=== Apply 中断 ===")
	for _, a := range res.Applied {
		fmt.Printf("  已完成 %s\n", a.ResourceID)
	}
	fmt.Printf("已完成 %d 个资源，修复故障后重新 plan && apply 将只处理剩余资源\n", len(res.Applied))
}

func printDrifts(drifts []engine.Drift) {
	if len(drifts) == 0 {
		fmt.Println("漂移检查: 无漂移")
		return
	}
	fmt.Printf("漂移检查: 发现 %d 处漂移\n", len(drifts))
	for _, d := range drifts {
		switch d.Reason {
		case "attributes-changed":
			fmt.Printf("  ! 漂移  %s: 属性被外部改动\n", d.ResourceID)
			for _, df := range d.Diffs {
				if !df.ActualExist {
					fmt.Printf("        属性 %s 在云端缺失，声明值为 %q\n", df.Attr, df.Desired)
				} else {
					fmt.Printf("        属性 %s: 声明=%q 云端实际=%q\n", df.Attr, df.Desired, df.Actual)
				}
			}
		case "deleted-externally":
			fmt.Printf("  ! 漂移  %s: 云端资源被外部删除（将重建）\n", d.ResourceID)
		case "unexpected-extra":
			fmt.Printf("  ! 漂移  %s: 云端存在但声明中没有的多余资源（不会自动删除）\n", d.ResourceID)
		}
}
}

func opCN(op engine.Op) string {
	switch op {
	case engine.OpCreate:
		return "创建"
	case engine.OpRecreate:
		return "重建"
	case engine.OpUpdate:
		return "更新"
	default:
		return string(op)
	}
}

func attrsString(attrs map[string]string) string {
	if len(attrs) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(attrs))
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, attrs[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
