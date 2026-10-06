package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// Store 基于本地 JSON 文件 + flock 实现状态持久化与互斥。
type Store struct {
	path string
	lock *os.File
}

const stateFileName = "orchestrator-state.json"
const lockFileName = "orchestrator-state.lock"

// NewStore 在指定工作目录上构建 Store。
func NewStore(workDir string) *Store {
	return &Store{path: filepath.Join(workDir, stateFileName)}
}

// TryLock 尝试获取排他锁；抢不到时立即返回 false（调用方可选择等待或失败）。
func (s *Store) TryLock(workDir string) (bool, error) {
	return s.lockImpl(workDir, false)
}

// Lock 阻塞直到拿到排他锁。
func (s *Store) Lock(workDir string) error {
	_, err := s.lockImpl(workDir, true)
	return err
}

func (s *Store) lockImpl(workDir string, block bool) (bool, error) {
	if s.lock != nil {
		return true, nil
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return false, fmt.Errorf("创建工作目录失败: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(workDir, lockFileName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, fmt.Errorf("打开锁文件失败: %w", err)
	}
	how := syscall.LOCK_EX
	if !block {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, fmt.Errorf("获取状态锁失败: %w", err)
	}
	s.lock = f
	return true, nil
}

// Unlock 释放排他锁。
func (s *Store) Unlock() error {
	if s.lock == nil {
		return nil
	}
	err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	if cerr := s.lock.Close(); err == nil {
		err = cerr
	}
	s.lock = nil
	return err
}

// Load 读取状态并做版本校验。文件不存在时返回一份全新状态。
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return newState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取状态文件失败: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("状态文件不是合法 JSON（可能被并发写坏或手工破坏）: %w", err)
	}
	if err := st.validate(); err != nil {
		return nil, err
	}
	return &st, nil
}

// Save 原子写状态：先写临时文件再 rename，持锁期间状态不会被交错改写。
func (s *Store) Save(st *State) error {
	if s.lock == nil {
		return ErrNotLocked
	}
	if err := st.validate(); err != nil {
		return err
	}
	st.Version = CurrentVersion

	ids := make([]string, 0, len(st.Resources))
	for id := range st.Resources {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	ordered := struct {
		Version         int                      `json:"version"`
		Resources       map[string]ResourceState `json:"resources"`
		PlanFingerprint string                   `json:"plan_fingerprint,omitempty"`
	}{Version: st.Version, Resources: map[string]ResourceState{}, PlanFingerprint: st.PlanFingerprint}
	for _, id := range ids {
		ordered.Resources[id] = st.Resources[id]
	}

	data, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化状态失败: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写临时状态文件失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("替换状态文件失败: %w", err)
	}
	return nil
}
