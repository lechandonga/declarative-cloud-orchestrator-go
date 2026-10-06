// Package engine 把声明、状态与模拟云粘合起来，提供 plan / refresh / apply 语义。
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lechandonga/declarative-cloud-orchestrator-go/internal/cloud"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/internal/manifest"
	"github.com/lechandonga/declarative-cloud-orchestrator-go/internal/state"
)

// Op 是计划动作类型。
type Op string

const (
	OpNoop     Op = "noop"
	OpCreate   Op = "create"
	OpUpdate   Op = "update"
	OpRecreate Op = "recreate" // 状态显示已创建但云端文件被外部删除
)

// Action 是针对单个资源的计划动作。
type Action struct {
	ResourceID string
	Kind       string
	Op         Op
	Desired    map[string]string
	Actual     map[string]string // 云端当前属性（可能为 nil）
	Diffs      []AttrDiff
}

// AttrDiff 描述单个属性的差异。
type AttrDiff struct {
	Attr        string
	Desired     string
	Actual      string
	ActualExist bool
}

// Drift 描述一份刷新结果中识别到的漂移。
type Drift struct {
	ResourceID string
	Kind       string
	Reason     string // attributes-changed / deleted-externally / unexpected-extra
	Diffs      []AttrDiff
}

// Plan 是一次计划的完整结果。
type Plan struct {
	Actions     []Action
	Fingerprint string
	Drifts      []Drift // apply 前基于云端实况发现的漂移
}

// ChangedCount 返回需要实际改动的资源数量。
func (p *Plan) ChangedCount() int {
	n := 0
	for _, a := range p.Actions {
		if a.Op != OpNoop {
			n++
		}
	}
	return n
}

// Engine 编排引擎。
type Engine struct {
	man *manifest.Manifest
	bk  *cloud.Backend
	st  *state.Store
}

func New(man *manifest.Manifest, bk *cloud.Backend, st *state.Store) *Engine {
	return &Engine{man: man, bk: bk, st: st}
}

func diffAttrs(desired, actual map[string]string) []AttrDiff {
	var diffs []AttrDiff
	keys := map[string]bool{}
	for k := range desired {
		keys[k] = true
	}
	for k := range actual {
		keys[k] = true
	}
	for k := range keys {
		dv, dok := desired[k]
		av, aok := actual[k]
		if dv != av || dok != aok {
			diffs = append(diffs, AttrDiff{Attr: k, Desired: dv, Actual: av, ActualExist: aok})
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Attr < diffs[j].Attr })
	return diffs
}

// compute 以云端实况为准计算动作列表；不做任何写操作。
func (e *Engine) compute() ([]Action, []Drift, error) {
	var actions []Action
	var drifts []Drift

	for _, id := range e.man.Order {
		r := e.man.Resources[id]
		existing, err := e.bk.Read(id)
		if err != nil && !strings.Contains(err.Error(), cloud.ErrNotFound.Error()) {
			return nil, nil, err
		}
		if err != nil {
			// 云端不存在
			actions = append(actions, Action{
				ResourceID: id, Kind: r.Kind, Op: OpCreate,
				Desired: cloneAttrs(r.Attrs),
			})
			continue
		}

		diffs := diffAttrs(r.Attrs, existing.Attrs)
		if len(diffs) == 0 {
			actions = append(actions, Action{
				ResourceID: id, Kind: r.Kind, Op: OpNoop,
				Desired: cloneAttrs(r.Attrs), Actual: cloneAttrs(existing.Attrs),
			})
			continue
		}

		op := OpUpdate
		reason := "attributes-changed"
		// 云端文件存在即按更新处理；属性变化本身即漂移。
		drifts = append(drifts, Drift{ResourceID: id, Kind: r.Kind, Reason: reason, Diffs: diffs})
		actions = append(actions, Action{
			ResourceID: id, Kind: r.Kind, Op: op,
			Desired: cloneAttrs(r.Attrs), Actual: cloneAttrs(existing.Attrs), Diffs: diffs,
		})
	}

	// 云端多出、声明里没有的资源：只报告漂移，不做删除等破坏性动作。
	cloudIDs, err := e.bk.List()
	if err != nil {
		return nil, nil, err
	}
	for _, cid := range cloudIDs {
		if _, ok := e.man.Resources[cid]; ok {
			continue
		}
		cr, err := e.bk.Read(cid)
		if err != nil {
			return nil, nil, err
		}
		drifts = append(drifts, Drift{ResourceID: cid, Kind: cr.Kind, Reason: "unexpected-extra"})
	}

	// 状态中存在、但云端文件被外部删除，且声明中也已移除的情况无法出现；
	// 声明仍在而云端缺失时上面已转为 create（首次）或 recreate（状态有记录）。
	st, err := e.st.Load()
	if err != nil {
		return nil, nil, err
	}
	for i := range actions {
		a := &actions[i]
		if a.Op == OpCreate {
			if _, recorded := st.Resources[a.ResourceID]; recorded {
				a.Op = OpRecreate
				drifts = append(drifts, Drift{ResourceID: a.ResourceID, Kind: a.Kind, Reason: "deleted-externally"})
			}
		}
	}

	sort.SliceStable(drifts, func(i, j int) bool {
		if drifts[i].ResourceID == drifts[j].ResourceID {
			return drifts[i].Reason < drifts[j].Reason
		}
		return drifts[i].ResourceID < drifts[j].ResourceID
	})
	return actions, drifts, nil
}

func fingerprint(actions []Action) string {
	type wireAction struct {
		ID      string            `json:"id"`
		Op      Op                `json:"op"`
		Desired map[string]string `json:"desired"`
	}
	wire := make([]wireAction, len(actions))
	for i, a := range actions {
		wire[i] = wireAction{ID: a.ResourceID, Op: a.Op, Desired: a.Desired}
	}
	data, _ := json.Marshal(wire)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Plan 计算计划并把指纹写入状态，保证之后 apply 必须与此刻一致。
func (e *Engine) Plan() (*Plan, error) {
	actions, drifts, err := e.compute()
	if err != nil {
		return nil, err
	}
	fp := fingerprint(actions)
	st, err := e.st.Load()
	if err != nil {
		return nil, err
	}
	st.PlanFingerprint = fp
	if err := e.st.Save(st); err != nil {
		return nil, err
	}
	return &Plan{Actions: actions, Fingerprint: fp, Drifts: drifts}, nil
}

// Refresh 只读刷新：读取云端实况并报告与状态/声明之间的漂移，不做任何改动。
func (e *Engine) Refresh() ([]Drift, error) {
	_, drifts, err := e.compute()
	return drifts, err
}

// ApplyResult 是执行结果。
type ApplyResult struct {
	Applied   []Action // 本次实际改动的资源（按执行顺序）
	Skipped   []Action // 已处于目标状态、未触碰的资源
	FailedOn  string   // 若非空，表示在该资源上失败
	FailedErr error
}

// Apply 执行编排。要求之前做过 plan；重新计算计划并校验指纹一致后才动手。
// 每完成一个资源就落一次状态（检查点），失败后重试只会处理剩余动作，
// 已完成的资源在云端与状态中均表现为 noop，绝不重复创建。
func (e *Engine) Apply() (*ApplyResult, error) {
	st, err := e.st.Load()
	if err != nil {
		return nil, err
	}

	actions, _, err := e.compute()
	if err != nil {
		return nil, err
	}
	fp := fingerprint(actions)
	if st.PlanFingerprint == "" {
		// 没有待执行计划：只有当一切已收敛（零改动）时允许幂等成功，
		// 任何需要改动的情况都必须先 plan，拒绝盲执行。
		if changed(actions) {
			return nil, fmt.Errorf("尚未生成计划：请先运行 plan 后再 apply")
		}
		return &ApplyResult{Skipped: actions}, nil
	}
	if fp != st.PlanFingerprint {
		return nil, fmt.Errorf("计划已过期：声明或云端状态自上次 plan 后发生了变化，请重新运行 plan 后再 apply")
	}

	res := &ApplyResult{}
	for _, a := range actions {
		switch a.Op {
		case OpNoop:
			res.Skipped = append(res.Skipped, a)
			continue
		case OpCreate, OpRecreate:
			err = e.bk.Create(cloud.Resource{ID: a.ResourceID, Kind: a.Kind, Attrs: a.Desired})
		case OpUpdate:
			err = e.bk.Update(cloud.Resource{ID: a.ResourceID, Kind: a.Kind, Attrs: a.Desired})
		default:
			return nil, fmt.Errorf("未知计划动作 %q", a.Op)
		}
		if err != nil {
			res.FailedOn = a.ResourceID
			res.FailedErr = err
			return res, fmt.Errorf("在资源 %q 上执行失败，已完成 %d 个资源，状态已检查点，可重试 apply 继续: %w",
				a.ResourceID, len(res.Applied), err)
		}

		st.Resources[a.ResourceID] = state.ResourceState{
			ID:     a.ResourceID,
			Kind:   a.Kind,
			Attrs: cloneAttrs(a.Desired),
			Status: string(a.Op),
		}
		if err := e.st.Save(st); err != nil {
			return nil, fmt.Errorf("资源 %q 已在云端创建，但状态检查点写入失败: %w", a.ResourceID, err)
		}
		res.Applied = append(res.Applied, a)
	}

	// 全部完成后清除指纹，重复 apply 会得到"无计划"而不是误执行。
	st.PlanFingerprint = ""
	if err := e.st.Save(st); err != nil {
		return nil, err
	}
	return res, nil
}

func changed(actions []Action) bool {
	for _, a := range actions {
		if a.Op != OpNoop {
			return true
		}
	}
	return false
}

func cloneAttrs(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
