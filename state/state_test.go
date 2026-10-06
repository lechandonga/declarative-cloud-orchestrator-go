package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStateVersionCompatibility(t *testing.T) {
	t.Log("业务场景: 状态版本兼容——未知或过旧版本必须明确报错而非按新语义解释")

	t.Run("过新版本报错", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "state.json")
		os.WriteFile(p, []byte(`{"version":99,"resources":{}}`), 0o644)
		_, err := NewStore(p).Load()
		var ve *VersionError
		if !errors.As(err, &ve) || ve.Found != 99 {
			t.Fatalf("expected VersionError(99), got %v", err)
		}
		t.Logf("用例通过: %v", err)
	})

	t.Run("过旧版本报错", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "state.json")
		os.WriteFile(p, []byte(`{"version":0,"resources":{}}`), 0o644)
		_, err := NewStore(p).Load()
		var ve *VersionError
		if !errors.As(err, &ve) {
			t.Fatalf("expected VersionError, got %v", err)
		}
		t.Logf("用例通过: %v", err)
	})

	t.Run("缺失状态文件初始化为当前版本", func(t *testing.T) {
		st, err := NewStore(filepath.Join(t.TempDir(), "state.json")).Load()
		if err != nil || st.Version != Version {
			t.Fatalf("got %v %v", st, err)
		}
		t.Log("用例通过: 空目录生成全新状态")
	})
}

func TestStateLockMutualExclusion(t *testing.T) {
	t.Log("业务场景: 并发执行——同一工作目录的状态锁互斥，抢不到则明确失败")
	p := filepath.Join(t.TempDir(), "state.json")
	s1, s2 := NewStore(p), NewStore(p)
	if err := s1.Lock(false); err != nil {
		t.Fatal(err)
	}
	defer s1.Unlock()
	if err := s2.Lock(false); !errors.Is(err, ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
	t.Log("用例通过: 非阻塞模式第二个执行者得到 ErrLocked")
	s1.Unlock()
	if err := s2.Lock(false); err != nil {
		t.Fatalf("lock should be free after unlock: %v", err)
	}
	s2.Unlock()
	t.Log("用例通过: 锁释放后可被有序获取")
}
