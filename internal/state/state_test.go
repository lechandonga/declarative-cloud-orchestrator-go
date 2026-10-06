package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 业务：状态版本兼容策略 —— 旧版本、未来版本都必须明确拒绝。
func TestState_VersionRejection(t *testing.T) {
	t.Log("业务[状态版本]: 过旧版本与未知新版本均不得按当前语义硬解释")
	dir := t.TempDir()

	store := NewStore(dir)
	ok, err := store.TryLock(dir)
	if !ok || err != nil {
		t.Fatalf("加锁失败: ok=%v err=%v", ok, err)
	}
	defer store.Unlock()

	for _, v := range []int{0, -1, 2, 99} {
		raw := `{"version":` + itoa(v) + `,"resources":{}}`
		if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := store.Load()
		if err == nil || !strings.Contains(err.Error(), "版本") {
			t.Fatalf("version=%d 时期望版本错误，得到 %v", v, err)
		}
		t.Logf("用例[拒绝版本 %d]: 通过，报错=%v", v, err)
	}

	// 当前版本可正常往返保存。
	st := newState()
	st.Resources["m.r"] = ResourceState{ID: "m.r", Kind: "k", Attrs: map[string]string{"a": "b"}, Status: "created"}
	if err := store.Save(st); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if got.Version != CurrentVersion || got.Resources["m.r"].Attrs["a"] != "b" {
		t.Fatalf("状态往返不一致: %+v", got)
	}
	t.Log("业务[状态版本]: 用例[当前版本往返保存] 通过")
}

// 业务：Save 必须持锁，防止无锁写造成交错。
func TestState_SaveRequiresLock(t *testing.T) {
	t.Log("业务[并发安全]: 未持锁写状态必须被拒绝")
	store := NewStore(t.TempDir())
	if err := store.Save(newState()); err != ErrNotLocked {
		t.Fatalf("期望 ErrNotLocked，得到 %v", err)
	}
	t.Log("用例[无锁写拒绝]: 通过")
}

func itoa(i int) string {
	// 简单实现，避免测试文件引入 strconv 的噪音
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
