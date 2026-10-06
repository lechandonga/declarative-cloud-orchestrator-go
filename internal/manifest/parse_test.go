package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("写文件 %s 失败: %v", name, err)
	}
}

// 业务：多模块拆分 + 引用解析 + 显式依赖，拓扑顺序必须让被依赖者在前。
func TestLoadDir_MultiModuleRefsAndOrder(t *testing.T) {
	t.Log("业务[声明解析]: 多文件模块、${mod.res.attr} 引用与显式依赖的拓扑排序")
	dir := t.TempDir()
	writeFile(t, dir, "net.yaml", `
module: net
resources:
  - name: vpc
    kind: network
    attrs:
      cidr: 10.0.0.0/16
`)
	writeFile(t, dir, "app.yml", `
module: app
resources:
  - name: web
    kind: function
    attrs:
      network_cidr: ${net.vpc.cidr}
      tag: web
    depends_on:
      - net.vpc
`)
	m, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("期望解析成功，得到错误: %v", err)
	}
	if got := m.Resources["app.web"].Attrs["network_cidr"]; got != "10.0.0.0/16" {
		t.Fatalf("引用未正确解析，network_cidr=%q", got)
	}
	if m.Order[0] != "net.vpc" || m.Order[1] != "app.web" {
		t.Fatalf("拓扑顺序错误: %v", m.Order)
	}
	t.Logf("用例[多模块引用排序]: 通过，执行顺序=%v", m.Order)
}

func TestLoadDir_Errors(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		wantSub string
	}{
		{
			name: "循环依赖被拒绝",
			files: map[string]string{
				"m.yaml": `
module: m
resources:
  - name: a
    kind: k
    depends_on: [m.b]
  - name: b
    kind: k
    depends_on: [m.a]
`,
			},
			wantSub: "循环依赖",
		},
		{
			name: "自依赖被拒绝",
			files: map[string]string{"m.yaml": `
module: m
resources:
  - name: a
    kind: k
    depends_on: [m.a]
`},
			wantSub: "循环依赖",
		},
		{
			name: "悬空显式依赖被拒绝",
			files: map[string]string{"m.yaml": `
module: m
resources:
  - name: a
    kind: k
    depends_on: [m.ghost]
`},
			wantSub: "悬空引用",
		},
		{
			name: "悬空属性引用-资源不存在",
			files: map[string]string{"m.yaml": `
module: m
resources:
  - name: a
    kind: k
    attrs:
      x: ${m.ghost.v}
`},
			wantSub: "悬空引用",
		},
		{
			name: "悬空属性引用-属性不存在",
			files: map[string]string{"m.yaml": `
module: m
resources:
  - name: a
    kind: k
    attrs: {x: v}
  - name: b
    kind: k
    attrs: {y: "${m.a.missing}"}
`},
			wantSub: "悬空引用",
		},
		{
			name: "重复定义被拒绝",
			files: map[string]string{"m.yaml": `
module: m
resources:
  - name: dup
    kind: k
  - name: dup
    kind: k
`},
			wantSub: "重复定义",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("业务[声明校验]: 用例[%s] —— 必须在执行前拒绝且报错原因可区分", tc.name)
			dir := t.TempDir()
			for name, content := range tc.files {
				writeFile(t, dir, name, content)
			}
			_, err := LoadDir(dir)
			if err == nil {
				t.Fatalf("期望报错（含 %q），但解析成功", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误原因不符：期望包含 %q，实际 %q", tc.wantSub, err.Error())
			}
			t.Logf("用例[%s]: 通过，报错=%v", tc.name, err)
		})
	}
}
