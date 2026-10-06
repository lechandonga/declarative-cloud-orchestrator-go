package state

import (
	"errors"
	"fmt"
)

// CurrentVersion 是当前引擎支持的唯一状态版本。
const CurrentVersion = 1

// MinSupportedVersion 是可读取的最低版本。旧版本必须显式拒绝。
const MinSupportedVersion = 1

// ErrNotLocked 表示未持锁就尝试写状态。
var ErrNotLocked = errors.New("状态文件未被当前进程锁定")

// ResourceState 记录单个资源的已落地状态。
type ResourceState struct {
	ID     string            `json:"id"`
	Kind   string            `json:"kind"`
	Attrs  map[string]string `json:"attrs"`
	Status string            `json:"status"` // created / updated / drift
}

// State 是持久化到工作目录的编排状态。
type State struct {
	Version   int                      `json:"version"`
	Resources map[string]ResourceState `json:"resources"`
	// PlanFingerprint 是最近一次 plan 的指纹，apply 时校验，
	// 防止"plan 的内容"与"apply 实际执行的内容"不一致。
	PlanFingerprint string `json:"plan_fingerprint,omitempty"`
}

func newState() *State {
	return &State{Version: CurrentVersion, Resources: map[string]ResourceState{}}
}

func (s *State) validate() error {
	if s.Version < MinSupportedVersion {
		return fmt.Errorf("状态版本 %d 过旧，最低支持版本为 %d；拒绝按新语义解释旧数据，请迁移或重建状态",
			s.Version, MinSupportedVersion)
	}
	if s.Version > CurrentVersion {
		return fmt.Errorf("状态版本 %d 来自更新版本的引擎，当前引擎最高支持版本 %d，拒绝读取",
			s.Version, CurrentVersion)
	}
	if s.Resources == nil {
		s.Resources = map[string]ResourceState{}
	}
	return nil
}
