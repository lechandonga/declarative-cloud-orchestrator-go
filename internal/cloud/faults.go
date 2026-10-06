package cloud

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// faultConfig 描述注入故障：对指定资源的指定操作，前 Remaining 次返回错误。
// 用于模拟 apply 中途失败，随后重试只能继续未完成部分。
type faultConfig struct {
	Remaining int    `json:"remaining"`
	Resource  string `json:"resource"`
	Operation string `json:"operation"`
}

var faultMu sync.Mutex

func faultPath(root string) string { return filepath.Join(root, "faults.json") }

// InjectFault 在模拟云根目录写入故障配置（供测试与演示使用）。
func InjectFault(root, resource, operation string, times int) error {
	faultMu.Lock()
	defer faultMu.Unlock()

	cfg := faultConfig{Remaining: times, Resource: resource, Operation: operation}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(faultPath(root), data, 0o644)
}

// ClearFault 删除故障配置。
func ClearFault(root string) error {
	faultMu.Lock()
	defer faultMu.Unlock()
	err := os.Remove(faultPath(root))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (b *Backend) checkFault(id, op string) error {
	faultMu.Lock()
	defer faultMu.Unlock()

	data, err := os.ReadFile(faultPath(b.root))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg faultConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("故障配置损坏: %w", err)
	}
	if cfg.Resource != id || cfg.Operation != op || cfg.Remaining <= 0 {
		return nil
	}
	cfg.Remaining--
	if err := os.WriteFile(faultPath(b.root), mustMarshal(cfg), 0o644); err != nil {
		return err
	}
	return fmt.Errorf("注入故障: 资源 %q 的 %s 操作失败（剩余 %d 次）", id, op, cfg.Remaining)
}

func mustMarshal(v faultConfig) []byte {
	data, _ := json.MarshalIndent(v, "", "  ")
	return data
}
