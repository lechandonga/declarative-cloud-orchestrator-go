package manifest

// Resource 是一条资源声明。
type Resource struct {
	// Module 是该声明所属的模块（来源文件名，不含目录）。
	Module string
	// Kind 是资源类型，如 bucket、function。
	Kind string
	// Name 是模块内资源名。
	Name string
	// Attrs 是资源属性的最终形态（默认值已合并、引用已解析）。
	Attrs map[string]string
	// rawAttrs 保留声明原文（可能含 ${module.res.attr} 引用），用于解析。
	rawAttrs map[string]string
	// DependsOn 是显式依赖，元素形如 "module.resource"。
	DependsOn []string
}

// ID 返回资源的全局唯一标识 "module.resource"。
func (r *Resource) ID() string {
	return r.Module + "." + r.Name
}

// Manifest 是一次编排涉及的全部资源声明。
type Manifest struct {
	Resources map[string]*Resource
	// Order 是按依赖拓扑排序后的资源 ID。
	Order []string
}
