// Package cloud 用本地文件模拟云端资源：每个资源对应 resources 目录下一个 JSON 文件。
// 对这些文件的任何手工修改，都等同于"云端资源被外部改动"，刷新时必须被识别为漂移。
package cloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound 表示云端不存在该资源。
var ErrNotFound = errors.New("云端资源不存在")

// Resource 是云端记录的资源形态。
type Resource struct {
	ID    string            `json:"id"`
	Kind  string            `json:"kind"`
	Attrs map[string]string `json:"attrs"`
}

// Backend 是基于本地文件的模拟云。同目录下多个 Backend 共享文件与故障配置。
type Backend struct {
	root string
	mu   sync.Mutex
}

// NewBackend 在 root 目录下创建模拟云（目录不存在则创建）。
func NewBackend(root string) (*Backend, error) {
	if err := os.MkdirAll(filepath.Join(root, "resources"), 0o755); err != nil {
		return nil, fmt.Errorf("初始化模拟云目录失败: %w", err)
	}
	return &Backend{root: root}, nil
}

func (b *Backend) resDir() string { return filepath.Join(b.root, "resources") }

func safePath(id string) (string, error) {
	if strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("非法资源 ID: %q", id)
	}
	return id + ".json", nil
}

// Create 创建资源；资源已存在则报错，防止重试时重复创建。
func (b *Backend) Create(r Resource) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.checkFault(r.ID, "create"); err != nil {
		return err
	}
	name, err := safePath(r.ID)
	if err != nil {
		return err
	}
	path := filepath.Join(b.resDir(), name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("云端已存在资源 %q，拒绝重复创建", r.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return b.write(path, r)
}

// Update 覆盖资源属性；资源不存在则报错。
func (b *Backend) Update(r Resource) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.checkFault(r.ID, "update"); err != nil {
		return err
	}
	name, err := safePath(r.ID)
	if err != nil {
		return err
	}
	path := filepath.Join(b.resDir(), name)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("更新 %q 失败: %w", r.ID, ErrNotFound)
		}
		return err
	}
	return b.write(path, r)
}

// Read 读取资源；文件被外部写坏时返回明确错误而不是静默忽略。
func (b *Backend) Read(id string) (*Resource, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	name, err := safePath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(b.resDir(), name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取 %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	var r Resource
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("云端资源 %q 的记录已损坏: %w", id, err)
	}
	if r.Attrs == nil {
		r.Attrs = map[string]string{}
	}
	return &r, nil
}

// List 列出云端全部资源 ID，按字典序。
func (b *Backend) List() ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	entries, err := os.ReadDir(b.resDir())
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

func (b *Backend) write(path string, r Resource) error {
	if r.Attrs == nil {
		r.Attrs = map[string]string{}
	}
	keys := make([]string, 0, len(r.Attrs))
	for k := range r.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := struct {
		ID    string            `json:"id"`
		Kind  string            `json:"kind"`
		Attrs map[string]string `json:"attrs"`
	}{ID: r.ID, Kind: r.Kind, Attrs: map[string]string{}}
	for _, k := range keys {
		ordered.Attrs[k] = r.Attrs[k]
	}
	data, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
