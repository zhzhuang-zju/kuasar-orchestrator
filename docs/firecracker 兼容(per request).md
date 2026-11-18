# Firecracker 兼容：从节点级到创建请求级

> 阶段性总结：截至 2026-11-18，已将 backend 从节点级固定选择改为创建请求级配置。同一个 Conductor 可以同时管理 Cloud Hypervisor（CH）和 Firecracker（FC）Sandbox。对外最终采用 reserved metadata 配置；backend 在创建接受时确定并持久化，此后不可变。

## 1. 本阶段目标与范围

上一阶段通过节点的 `sandbox.backend` 决定整个节点使用 CH 还是 FC，主要验证 FC 冷启动执行链路。本阶段不重做 VMM 适配，而是调整**选择入口、配置校验、生命周期判断与重启恢复**，让每个 Sandbox 独立选择 backend。

核心语义：**创建时可选，创建后不可变；节点配置只提供默认值。**

| 对比项 | 原 node-level 实现 | 当前 per-request 实现 |
| --- | --- | --- |
| 选择入口 | 节点 `sandbox.backend` | Create 请求 metadata；未指定时采用节点默认值 |
| 同一 Conductor 混跑 CH / FC | 不支持 | 支持，已实测 |
| 生命周期能力判断 | 多处依赖节点 backend | 按目标 Sandbox 已保存的 backend 判断 |
| 修改节点默认值 | 有不兼容非终态 Sandbox 时阻止恢复 | 不改变已有 Sandbox 的选择，允许混合恢复；非法持久状态仍拒绝 |
| Builder | FC 节点默认下整体拒绝 | Builder 显式使用 CH，不随普通 Sandbox 默认值改变 |
| 底层 runtime | 已有 CH / FC 分支 | 复用上一阶段分支，无新增 VMM 协议 |

## 2. 最终请求配置

### 2.1 显式选择 backend

向 Conductor 的 `POST /sandboxes` 提交：

```json
{
	"templateID": "<prepared-image-template-id>",
	"timeout": 3600,
	"metadata": {
		"kuasar-sandbox.backend": "{\"name\":\"firecracker\"}"
	}
}
```

选择 CH 时，将 `name` 改为 `cloud-hypervisor`。由于 metadata 是 `map[string]string`，该值是**包含 JSON 对象的字符串**，不是直接嵌套对象。

| 输入情况 | 处理行为 |
| --- | --- |
| `name=firecracker` | 选择 FC，继续执行 FC 能力与资源校验 |
| `name=cloud-hypervisor` | 选择 CH |
| 未提供 `kuasar-sandbox.backend` | 使用节点 `sandbox.backend`；节点也未配置时默认 CH |
| 提供空字符串、`null`、空对象、非法类型或不支持的名称 | 拒绝，不当作默认值处理 |
| 对象存在重复字段、未知字段或尾随 JSON | 拒绝 |
| 使用先前试验中的顶层 `backend` 字段 | 拒绝，包括空值和 `null`，避免旧请求被静默忽略 |

当前没有 backend HTTP header 别名，也不从模板或 Build metadata 继承 backend 默认值。

### 2.2 选择不等于资源配置转换

选择 FC 不会自动将 CH 的弹性资源默认值改成静态配置。节点默认值不满足 FC 限制时，需要显式提供资源 patch，例如：

```json
{
	"templateID": "<prepared-image-template-id>",
	"timeout": 3600,
	"metadata": {
		"kuasar-sandbox.backend": "{\"name\":\"firecracker\"}",
		"kuasar-sandbox.resource": "{\"capacity\":{\"cpu\":1,\"memory\":\"384MiB\"},\"allocatable\":{\"memory\":\"384MiB\"}}"
	}
}
```

FC 仍要求最终 capacity、allocatable 和 startup 内存相等，且不支持动态内存控制。如果节点启用 `resource_listen.enabled`，当前会拒绝 FC 请求；不能将“同节点支持两种 backend”理解为两种 backend 已具备相同能力。

## 3. 输入、持久化与执行链路

```text
POST /sandboxes
	→ 解析并消费 metadata 中的公共 backend 配置
	→ 解析节点默认值，形成内部 typed Backend
	→ 可选 Create Hook 调整；核心重新校验
	→ 检查所选 backend 的模板、资源及生命周期限制
	→ 接受创建，持久化最终 backend
	→ runner 取得可信启动规格
	→ sandbox-ctl run --backend <已保存的选择>
	→ 启动对应 VMM
```

### 3.1 三个不同层次的 backend 字段

| 层次 | 表示 | 作用 |
| --- | --- | --- |
| 公共请求 | `metadata["kuasar-sandbox.backend"]` | 用户仅在创建时提出选择；解析后消费，不作为普通标签继续传播 |
| 内部请求 / Hook / 节点命令 | typed `Backend` | 在可信内部结构中传递已解析选择；不是重新开放公共顶层字段 |
| 持久状态 | `metadata["kuasar-sandbox.internal.backend"]` | 核心保存最终结果，供启动、生命周期及恢复读取；用户不得直接写入 |

Create Hook 可以在接受创建之前修改 typed `Backend`，但修改后仍需通过核心校验，不能重新注入公共 metadata 作为第二个选择来源。不可变约束从**创建被接受**开始生效。

历史 Sandbox 未保存内部 backend 时，明确解释为 CH，而不是读取节点当前默认值。这样默认值变化不会把历史 CH Sandbox 错误解释为 FC。

### 3.2 集群路径

Router 在公共 Create 入口解析 metadata，规范化选择依次经过 Registry reservation、持久化路由/重试流程、node-link Create 命令到达目标 Conductor。

本阶段为这条传播链补充了字段、校验和测试；非 Create 操作不能借内部命令重新选择 backend。**尚未执行真实多节点集群部署验收**，集群部分的证据来自自动化测试。

## 4. 主要代码改动

| 组件 | 本阶段改动 |
| --- | --- |
| 配置解析 | 增加公共 backend metadata 的严格解析、消费与内部字段保护；拒绝旧顶层选择器 |
| Create API / 扩展 Hook | 将公共输入规范化为 typed Backend；重新校验 Hook 结果；保持请求数据隔离 |
| Conductor 创建与启动 | 持久化最终选择，使用 Sandbox 自身 backend 生成启动参数并校验资源 |
| 生命周期与恢复 | 将节点级条件改为 Sandbox 级条件；允许默认值变化和混合 backend 恢复；拒绝 FC 非法暂停/恢复状态 |
| TTL 与压力处理 | FC 到期走 Delete，避免调用未支持的 Pause；压力候选筛选避免选择 FC 执行快照暂停 |
| Builder / 导入 | Build 显式走 CH；受支持的 CH artifact 导入固定为 CH，不随节点默认值变成 FC |
| Router / Registry / routesync | 规范化 backend 在集群链路传播；覆盖持久化、重试、替换与非 Create 拒绝 |
| 测试与文档 | 增加 metadata、API、Hook、集群传播、不可变状态与混合恢复测试；更新中英文契约及配置示例 |

主要入口：

- [backend metadata 解析](../internal/sandboxcfg/backend.go)
- [Create API](../internal/api/api.go)
- [创建及持久化](../internal/orch/create.go)
- [backend 生命周期与恢复判断](../internal/orch/backend.go)
- [runner 启动规格校验](../cmd/node-ctl/taskrun.go)
- [集群协议](../internal/routesync/cluster.go)

**相对上一阶段，新增代码改动集中在 orchestrator 仓库。** sandboxer 和 guest-runtime 复用已完成的 FC 适配；可运行环境仍依赖那两个仓库的匹配版本，不能换回不支持 FC 的旧二进制。

## 5. 验证进度与证据边界

### 5.1 自动化测试

- 相关 API、配置、扩展 Hook、Conductor、Builder、runner、Router、Registry、routesync 测试通过。
- metadata 输入迁移后，仓库全量 Go 测试和 backend 定向 race 测试通过；另行复跑了 backend 解析与传播的定向测试。
- 覆盖非法输入、旧字段拒绝、内部 metadata 保护、默认值解析、持久化不可变选择、资源限制及混合恢复等场景。
- 不等同于全量产品 E2E、故障注入认证或生产容量验证。

### 5.2 真实混合 backend 与默认值变化

2026-11-17，在迁移到 metadata 输入之前，使用过渡版顶层选择器完成以下真实测试：

1. 同一 Conductor 创建 CH 和 FC 两个 Sandbox，两个 VMM 同时运行。
2. 两者均通过 Proxy 原生 exec、Guest 文件写入和读取。
3. 保留两个运行实例，将节点默认值从 CH 改为 FC，再重启 Conductor。
4. 原有两个实例保持可访问，未因默认值变化切换 backend。
5. 删除后确认 API 404、runner/VMM 清理及 64 个交换机端口释放。

这证明了该版本的后端持久化及混合恢复。**尚未对最终 metadata 版本重复同一默认值切换/重启实验**，不能混淆两个输入版本的验证记录。

### 5.3 最终 metadata 版本的部署与创建

2026-11-18，将 metadata 版本部署到运行中的 Debian worker，并使用最终请求格式创建：

| backend | 实验 Sandbox ID | 结果 |
| --- | --- | --- |
| CH | `01a1ec5b-4ad9-7d79-9101-6d2fc198226b` | `running`；持久化 backend 正确；Proxy exec 和文件读写通过 |
| FC | `01a1ec5b-4b4a-7b1b-b57e-0a2a5633b2f1` | `running`；持久化 backend 正确；Proxy exec 和文件读写通过 |

同时确认两个 VMM 进程存在；旧顶层 backend 和非法 metadata backend 被拒绝。这两个实例设置一小时 TTL，随后已确认运行结束，ID 仅用于追溯实验。

### 5.4 PicoD 作为主应用的双后端验证

随后准备了包含 PicoD 和验签公钥的新 Alpine ARM64 image 模板。启动入口加载公钥后通过 `exec` 启动 PicoD，不再先运行 sleep 再注入后台服务。

| backend | 实验 Sandbox ID | 结果 |
| --- | --- | --- |
| CH | `01a1ec9f-8683-78ec-bb47-7d396154ecf7` | PicoD 为应用 PID 1；Proxy 健康检查、JWT 命令执行与文件读写通过 |
| FC | `01a1ec9f-8708-77dc-86cc-6f0cef2fc5ff` | PicoD 为应用 PID 1；Proxy 健康检查、JWT 命令执行与文件读写通过 |

PicoD 监听 Guest 8081，Proxy 入口为 worker 18084；请求同时使用 Kuasar forwarding token 和 PicoD JWT。私钥不进入镜像，也不提交仓库。

首次 HTTP 探测曾在 PicoD 监听就绪前返回 502，后续通过。说明 bare Sandbox 的 `running` 表示 runtime/应用启动协议完成，**不保证应用 HTTP listener 已经可用**，调用方仍需应用健康探测。

本轮双后端 PicoD 测试验证健康和命令执行/文件写读；之前的 FC PicoD 专项实验还验证了文件 API 和鉴权异常。不能由此宣称完整 AgentCube Router/SDK/Kubernetes 集成已通过。

上述实例均设置一小时 TTL，PicoD 实例的记录到期时间为 2026-11-18 04:07:20 UTC。此处是历史证据，不代表当前服务或实例仍存活。

## 6. 未改变的能力边界

- FC 动态内存、快照/恢复、Pause/Resume、Reset、导入/导出、迁移和 Workspace 等限制没有因 per-request 选择而解除。
- Builder 可以在 FC 默认节点上运行，但实际构建 VMM 仍为 CH，并非实现了 FC Builder。
- 当前只有内置 CH / FC 选择，不是通用可执行插件框架，也没有新增 `allowed_backends` 配置。
- Guest kernel/runtime 和模板必须与所选执行路径兼容；backend 选择不负责把任意容器镜像自动变为模板。
- 本地验证为 ARM64 QEMU TCG 功能测试，不代表 x86_64、原生 KVM 性能或生产稳定性结论。
- Guest 出网/DNS、完整 E2B/Envd 和完整 AgentCube SDK 链路不在本阶段验收范围。
- Debian lab 使用 transient systemd 服务，不是重启自动恢复的生产部署。镜像、实验脚本、密钥和 VM 磁盘不由代码 commit 自动保存。

## 7. 小结与相关文档

本阶段完成了从“节点固定后端”到“**请求提出选择、核心校验、接受时固化、生命周期按实例处理**”的改造，并收敛到与项目现有风格一致的 metadata 输入方式。已实测同节点双后端创建及 PicoD 主应用访问；完整混合恢复有过渡版实测，最终 metadata 版本的重启复测与真实多节点验收仍需单独补充。

- [上一阶段：节点级适配记录](firecracker%20兼容(node%20level).md)
- [当前 Firecracker 功能说明](firecracker.md)
- [PicoD 专项测试记录](picod-firecracker-test.md)
- [Sandbox 生命周期契约](sandbox-lifecycle_zh.md)
