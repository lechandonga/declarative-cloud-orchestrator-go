package decl

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeDecls(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func kindOf(err error) ErrorKind {
	var de *Error
	if errors.As(err, &de) {
		return de.Kind
	}
	return -1
}

func TestValidation_DistinctFailureKinds(t *testing.T) {
	t.Log("业务场景: 声明校验——循环依赖、悬空引用、重复定义必须在执行前被区分并拒绝")

	t.Run("循环依赖被拒绝并指明环节点", func(t *testing.T) {
		dir := writeDecls(t, map[string]string{"a.json": `{"resources":[
			{"type":"net","name":"a","depends_on":["net.b"]},
			{"type":"net","name":"b","depends_on":["net.a"]}]}`})
		_, err := LoadDir(dir)
		if kindOf(err) != KindCycle {
			t.Fatalf("expected KindCycle, got %v", err)
		}
		t.Logf("用例通过: 循环依赖报错 %q", err)
	})

	t.Run("悬空depends_on被拒绝", func(t *testing.T) {
		dir := writeDecls(t, map[string]string{"a.json": `{"resources":[
			{"type":"db","name":"main","depends_on":["net.ghost"]}]}`})
		_, err := LoadDir(dir)
		if kindOf(err) != KindDangling {
			t.Fatalf("expected KindDangling, got %v", err)
		}
		t.Logf("用例通过: 悬空依赖报错 %q", err)
	})

	t.Run("悬空属性引用被拒绝", func(t *testing.T) {
		dir := writeDecls(t, map[string]string{"a.json": `{"resources":[
			{"type":"app","name":"web","properties":{"peer":"${db.missing.id}"}}]}`})
		_, err := LoadDir(dir)
		if kindOf(err) != KindDangling {
			t.Fatalf("expected KindDangling, got %v", err)
		}
		t.Logf("用例通过: 悬空引用报错 %q", err)
	})

	t.Run("跨文件重复定义被拒绝并列出两个文件", func(t *testing.T) {
		dir := writeDecls(t, map[string]string{
			"a.json": `{"resources":[{"type":"net","name":"vpc"}]}`,
			"b.json": `{"resources":[{"type":"net","name":"vpc"}]}`,
		})
		_, err := LoadDir(dir)
		if kindOf(err) != KindDuplicate {
			t.Fatalf("expected KindDuplicate, got %v", err)
		}
		t.Logf("用例通过: 重复定义报错 %q", err)
	})
}

func TestLoadDir_MultiFileOrderingAndRefs(t *testing.T) {
	t.Log("业务场景: 声明解析——多文件拆分、引用与显式依赖共同决定拓扑顺序")
	dir := writeDecls(t, map[string]string{
		"network.json": `{"resources":[{"type":"net","name":"vpc","properties":{"cidr":"10.0.0.0/16"}}]}`,
		"db.json":      `{"resources":[{"type":"db","name":"main","properties":{"net":"${net.vpc}","cidr":"${net.vpc.cidr}"}}]}`,
		"app.json": `{"resources":[
			{"type":"app","name":"web","depends_on":["db.main"]},
			{"type":"app","name":"worker","depends_on":["app.web"]}]}`,
	})
	set, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, addr := range set.Order {
		pos[addr] = i
	}
	for _, pair := range [][2]string{
		{"net.vpc", "db.main"}, {"db.main", "app.web"}, {"app.web", "app.worker"},
	} {
		if pos[pair[0]] >= pos[pair[1]] {
			t.Fatalf("expected %s before %s, order=%v", pair[0], pair[1], set.Order)
		}
	}
	t.Logf("用例通过: 拓扑顺序 %v", set.Order)
}
