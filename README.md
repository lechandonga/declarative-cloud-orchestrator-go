# declarative-cloud-orchestrator-go

一套本地可运行的声明式云资源编排引擎：资源声明是本地 JSON 文件，"云端" 也是本地目录中的 JSON 文件，因此计划（plan）、执行（apply）、失败重试、并发互斥与漂移处理的结果完全可预期、可复现。

## 快速开始

```bash
go build -o orchestrator .
./orchestrator -dir examples/decls -state demo/state.json -cloud demo/cloud plan
./orchestrator -dir examples/decls -state demo/state.json -cloud demo/cloud apply
./orchestrator -dir examples/decls -state demo/state.json -cloud demo/cloud apply   # 再次执行: No changes
```

全局参数：`-dir` 声明目录（默认 `decls`）、`-state` 状态文件（默认 `state.json`）、`-cloud` 模拟云目录（默认 `cloud`）、`-wait` 抢不到状态锁时有序等待（默认立即失败）。

## 声明格式

声明目录下所有 `*.json` 文件共同构成一份声明（可按模块拆分为多个文件），每个文件包含 `resources` 数组：

```json
{"resources": [
  {"type": "db", "name": "main",
   "properties": {"cidr": "${net.vpc.cidr}", "size": 2},
   "depends_on": ["net.vpc"]}
]}
```

- 资源地址为 `type.name`，全局唯一，跨文件重复定义会被拒绝（`duplicate resource`，报出两个来源文件）。
- 属性字符串中可写 `${type.name}`（整体引用，建立隐式依赖）或 `${type.name.attr}`（解析为被引用资源已落库属性的值，支持嵌套路径）。
- `depends_on` 声明显式依赖。隐式引用与显式依赖共同构成依赖图，按拓扑序执行（同层按地址字母序，保证确定性）。
- 以下问题在执行前整体拒绝，报错类型可区分（`decl.Error.Kind`），不静默跳过、不执行一半：
  - `KindCycle` 循环依赖（报出环上所有资源）
  - `KindDangling` 悬空引用 / 悬空 `depends_on`
  - `KindDuplicate` 重复定义
  - `KindParse` 文件格式错误

## 状态模型与版本兼容

状态文件（默认 `state.json`）记录编排器视角下云端应有的样子：

```json
{"version": 1, "serial": 7,
 "resources": {"net.vpc": {"type": "net", "name": "vpc", "properties": {...}}}}
```

- `version`：状态模式版本，当前唯一支持 `1`（`state.Version`）。
- `serial`：每次落盘递增，用于观察写次数。
- 版本兼容策略：**只接受完全匹配的版本**。更高版本报 "newer than supported"，更低版本报 "too old ... run migration"，均为 `state.VersionError`，绝不按新语义硬解释旧数据。状态文件不存在时初始化为当前版本的空状态。
- 状态写盘为「临时文件 + rename」原子替换；apply 期间每完成一个资源就检查点落盘一次，崩溃后重试只继续未完成部分。

## 计划与执行语义

- **refresh**：在持锁状态下逐个读取状态中的资源并与云端实况比对。被外部删除的资源记为 `deleted` 漂移；属性不一致记为 `modified` 漂移并列出逐属性差异；状态同步为实况。
- **plan**：先 refresh（漂移随计划一并展示，不会被当成无变更），再计算收敛到声明所需的变更：声明中有而状态中没有 → `create`；属性不同 → `update`（带属性级 diff）；状态中有而声明中删除 → `delete`；否则 `noop`。顺序为拓扑序（删除在末尾）。
- **apply**：在锁内重新计算 plan 并严格执行，保证「plan 展示什么就做什么」。每个资源完成后立即检查点状态。中途失败时返回错误，已完成的资源保持已落库状态；重试时 refresh 会让已完成部分显示为 `noop`，只执行剩余变更——不重复创建、不漏资源。
- **幂等**：无任何变化时 apply 全部为 `noop`，不触碰任何云端对象。
- **并发**：同一状态文件用 `flock` 互斥。默认非阻塞，抢不到锁立即报 `state is locked by another process`；加 `-wait` 则有序排队等待。等待者获取锁后读到的是前者完整落盘的状态，不会交错改写。

## 本地模拟云

`cloud/` 目录下每个资源一个文件 `<type>.<name>.json`，内容为 `{type, name, properties}`。直接编辑/删除这些文件即可模拟外部改动，随后 `plan`/`refresh` 会报告漂移，`apply` 会把资源纠正回声明状态。测试可通过 `cloud.Client.InjectFailure(op, addr, err)` 注入一次性失败来模拟执行中途崩溃。

## 本地验证

```bash
go test ./...        # 全量测试
go test ./... -v     # 日志中标注了各业务场景与用例通过情况
```

测试覆盖的业务场景（`go test -v` 日志可见）：

- 声明校验：循环依赖 / 悬空引用 / 重复定义的分类拒绝（`decl`）
- 多文件拆分、引用与显式依赖的拓扑排序（`decl`）
- plan 与 apply 一致性、重复 apply 幂等（`engine`）
- 失败注入后的重试续跑：不重复创建、不遗漏（`engine`）
- 并发执行的锁互斥与有序等待（`engine`、`state`）
- 外部改动后的漂移识别与纠正（`engine`）
- 状态版本过新 / 过旧的明确报错（`state`）
