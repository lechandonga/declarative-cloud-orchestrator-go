package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// rawModule 对应一个声明文件的 YAML 结构。
type rawModule struct {
	Module    string                   `yaml:"module"`
	Resources []map[string]interface{} `yaml:"resources"`
}

var refPattern = regexp.MustCompile(`\$\{([a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)\}`)

// LoadDir 读取目录下全部 *.yaml/*.yml 声明文件并构建、校验清单。
// 文件名（去掉扩展名）即模块名，文件内 module 字段必须与文件名一致或留空。
func LoadDir(dir string) (*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取声明目录失败: %w", err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".yaml" || ext == ".yml" {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	resources := map[string]*Resource{}

	for _, fname := range files {
		modName := strings.TrimSuffix(strings.TrimSuffix(fname, filepath.Ext(fname)), "")
		data, err := os.ReadFile(filepath.Join(dir, fname))
		if err != nil {
			return nil, fmt.Errorf("读取声明文件 %s 失败: %w", fname, err)
		}
		var raw rawModule
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("解析声明文件 %s 的 YAML 失败: %w", fname, err)
		}
		if raw.Module != "" && raw.Module != modName {
			return nil, fmt.Errorf("模块名不一致: 文件 %s 声明的 module 为 %q，按文件名应为 %q",
				fname, raw.Module, modName)
		}
		for idx, item := range raw.Resources {
			r, err := decodeResource(modName, item)
			if err != nil {
				return nil, fmt.Errorf("文件 %s 第 %d 个资源: %w", fname, idx+1, err)
			}
			id := r.ID()
			if _, exists := resources[id]; exists {
				return nil, fmt.Errorf("重复定义: 资源 %q 在模块 %q 中被声明了多次", r.Name, r.Module)
			}
			resources[id] = r
		}
	}

	if len(resources) == 0 {
		return nil, fmt.Errorf("声明目录 %s 中没有任何资源", dir)
	}

	if err := resolveRefs(resources); err != nil {
		return nil, err
	}

	order, err := topoSort(resources)
	if err != nil {
		return nil, err
	}

	return &Manifest{Resources: resources, Order: order}, nil
}

func decodeResource(module string, item map[string]interface{}) (*Resource, error) {
	nameVal, ok := item["name"]
	if !ok {
		return nil, fmt.Errorf("缺少 name 字段")
	}
	name, ok := nameVal.(string)
	if !ok || name == "" {
		return nil, fmt.Errorf("name 必须是非空字符串")
	}
	kindVal, ok := item["kind"]
	if !ok {
		return nil, fmt.Errorf("资源 %q 缺少 kind 字段", name)
	}
	kind, ok := kindVal.(string)
	if !ok || kind == "" {
		return nil, fmt.Errorf("资源 %q 的 kind 必须是非空字符串", name)
}

	r := &Resource{
		Module:   module,
		Kind:     kind,
		Name:     name,
		Attrs:    map[string]string{},
		rawAttrs: map[string]string{},
	}

	if attrs, ok := item["attrs"].(map[string]interface{}); ok {
		for k, v := range attrs {
			s, err := stringify(v)
			if err != nil {
				return nil, fmt.Errorf("资源 %q 的属性 %q: %w", name, k, err)
			}
			r.rawAttrs[k] = s
		}
	}

	switch d := item["depends_on"].(type) {
	case []interface{}:
		for _, v := range d {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("资源 %q 的 depends_on 必须是字符串列表", name)
			}
			r.DependsOn = append(r.DependsOn, s)
		}
	case []string:
		r.DependsOn = append(r.DependsOn, d...)
	case nil:
	default:
		return nil, fmt.Errorf("资源 %q 的 depends_on 必须是列表", name)
	}

	return r, nil
}

func stringify(v interface{}) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case int:
		return fmt.Sprintf("%d", t), nil
	case bool:
		return fmt.Sprintf("%t", t), nil
	default:
		return "", fmt.Errorf("属性值必须是字符串/数字/布尔值，得到 %T", v)
	}
}

// resolveRefs 解析属性中的 ${mod.res.attr} 引用，并收集引用形成的隐式依赖。
func resolveRefs(resources map[string]*Resource) error {
	for id, r := range resources {
		for _, dep := range r.DependsOn {
			if _, ok := resources[dep]; !ok {
				return fmt.Errorf("悬空引用: 资源 %q 的显式依赖 %q 不存在", id, dep)
			}
			if dep == id {
				return fmt.Errorf("循环依赖: 资源 %q 不能依赖自身", id)
			}
		}
	}

	for id, r := range resources {
		keys := make([]string, 0, len(r.rawAttrs))
		for k := range r.rawAttrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			raw := r.rawAttrs[k]
			resolved := raw
			matches := refPattern.FindAllStringSubmatch(raw, -1)
			for _, m := range matches {
				ref := m[1]
				parts := strings.SplitN(ref, ".", 3)
				targetID := parts[0] + "." + parts[1]
				targetAttr := parts[2]
				target, ok := resources[targetID]
				if !ok {
					return fmt.Errorf("悬空引用: 资源 %q 的属性 %q 引用了不存在的资源 %q", id, k, targetID)
				}
				tv, ok := target.rawAttrs[targetAttr]
				if !ok {
					return fmt.Errorf("悬空引用: 资源 %q 的属性 %q 引用了 %q 上不存在的属性 %q",
						id, k, targetID, targetAttr)
				}
				if nested := refPattern.FindAllStringSubmatch(tv, -1); len(nested) > 0 {
					return fmt.Errorf("不支持嵌套引用: 资源 %q 的属性 %q 间接依赖引用 %q", id, k, nested[0][1])
				}
				resolved = strings.ReplaceAll(resolved, m[0], tv)
				r.DependsOn = appendIfMissing(r.DependsOn, targetID)
			}
			r.Attrs[k] = resolved
		}
	}
	return nil
}

func appendIfMissing(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// topoSort 使用 Kahn 算法拓扑排序；环上的资源会被明确报出。
func topoSort(resources map[string]*Resource) ([]string, error) {
	indeg := map[string]int{}
	dependents := map[string][]string{}
	for id := range resources {
		indeg[id] = 0
	}
	for id, r := range resources {
		seen := map[string]bool{}
		for _, dep := range r.DependsOn {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			indeg[id]++
			dependents[dep] = append(dependents[dep], id)
		}
	}

	var ready []string
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	var order []string
	for len(ready) > 0 {
		cur := ready[0]
		ready = ready[1:]
		order = append(order, cur)
		nexts := append([]string{}, dependents[cur]...)
		sort.Strings(nexts)
		for _, next := range nexts {
			indeg[next]--
			if indeg[next] == 0 {
				ready = append(ready, next)
				sort.Strings(ready)
			}
		}
	}

	if len(order) != len(resources) {
		var cycle []string
		for id, d := range indeg {
			if d > 0 {
				cycle = append(cycle, id)
			}
		}
		sort.Strings(cycle)
		return nil, fmt.Errorf("循环依赖: 以下资源构成依赖环，无法确定执行顺序: %s", strings.Join(cycle, ", "))
	}
	return order, nil
}
