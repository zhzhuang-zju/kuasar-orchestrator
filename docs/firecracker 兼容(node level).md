# Firecracker 兼容

> 阶段性总结：截至 2026-11-17，已完成节点级 Firecracker 冷启动后端适配，并在本地 ARM64 仿真环境中验证创建、执行命令、文件读写和删除闭环。当前不是 Cloud Hypervisor 的完整功能替代。

## 1. 目标与当前结论

保留 Conductor 作为 Sandbox Management，复用现有 API 鉴权、状态管理、runner pool、systemd 监管和 Proxy 访问链路，在 `sandbox-ctl` 内增加 Firecracker 启动路径。

逻辑调用链如下；它表示任务交接关系，不是严格的父子进程关系，runner 由 systemd 启动和管理：

```text
用户 / SDK
	→ Conductor：鉴权、准入、持久化状态、分配 runner
	→ systemd runner：取得可信启动规格，传递 cgroup / readiness FD
	→ sandbox-ctl run：按 backend 选择启动路径
			├─ Cloud Hypervisor → microVM
			└─ Firecracker      → microVM
```

当前阶段的结论：

- **已实现并验证**：Firecracker bare image 冷启动、就绪确认、通过 Proxy 鉴权执行命令、Guest 文件读写、删除及资源回收。
- **保留兼容**：Cloud Hypervisor 为默认后端；修改后的组件通过了同类真实生命周期回归测试。
- **尚未实现**：Firecracker 动态内存、快照恢复等扩展能力。
- **仅完成设计讨论**：创建请求级后端选择、可插拔 backend registry；不能将这些方案视为当前已有功能。

## 2. 当前实现：节点级选择（node level）

### 2.1 配置方式

节点通过 Conductor 的 `sandbox.backend` 配置确定 runtime 后端，取值为 `cloud-hypervisor` 或 `firecracker`；省略时使用 Cloud Hypervisor。

下面是 Firecracker 的关键配置片段，不是完整部署配置：

```yaml
sandbox:
	backend: firecracker
	firecracker_binary: /opt/kuasar/bin/firecracker
	resources:
		capacity: {cpu: 1, memory: 384MiB}
		allocatable: {cpu: 1, memory: 384MiB}
```

当前 Firecracker 路径要求：

- CPU capacity 为 1～32；内存为正整数 MiB。
- 最终解析的 `allocatable.memory`、`startup.memory` 必须等于 `capacity.memory`，使用静态内存配置。
- `startup.memory` 可以省略，此时继承 capacity；`allocatable.memory` 建议显式配置，因为节点默认值通常为 256 MiB，而不是 capacity。
- 禁用动态节点资源控制器 `resource_listen`。
- 提供架构匹配的 Firecracker、Guest kernel、适配后的 runtime bundle 和可用 image 模板。

### 2.2 选择与恢复边界

- backend 在 Conductor 启动时读取，**不支持运行时热切换**。
- 接受创建后，将 backend 记录到 Sandbox 持久化 metadata，后续启动规格使用该记录；调用方不能通过任意 metadata 覆盖它。
- 历史记录没有 backend 时，按 Cloud Hypervisor 解释。
- 恢复时检查节点配置与非终态 Sandbox 的 backend 是否一致，避免错误接管已有执行。
- 切换节点 backend 前，需要清理已有非终态 Sandbox；切到 Firecracker 时还需处理非终态 Build 和待执行 Reset 等不兼容状态。
- 不能把已经运行的 Cloud Hypervisor VM 原地转换成 Firecracker VM。

实现说明见 [后端配置校验](config/backend.go)、[生命周期与恢复检查](internal/orch/backend.go)和[运行任务交接](cmd/node-ctl/taskrun.go)。

## 3. 功能适配状态与可行性

“尚未适配”指当前适配器未支持，不代表 Firecracker 本身没有该能力。“未验证”也不等于已确认不支持。

| 功能 | 当前状态 | 实现范围 / 后续可行性分析 |
| --- | --- | --- |
| 冷启动创建 image Sandbox | 已适配、已验证 | 复用 Conductor 创建与 runner 分配流程，通过 Firecracker API 配置并启动 VM；当前验证 bare Alpine ARM64。 |
| 就绪与状态提交 | 已适配、已验证 | 保留 `control_ready` → `ready` → EOF 协议；不是看到 VMM 进程或收到 Create 成功响应就认为 Guest 已就绪。 |
| 原生命令执行 | 已适配、已验证 | 复用 guestlink / vsock 和 Proxy exec 鉴权，真实 Guest 命令退出码为 0。 |
| Guest 文件读写 | 已验证 | 已通过 Guest 命令写入并读取文件；不代表所有 SDK 文件服务都已验证。 |
| 删除与失败清理 | 已适配；成功路径已验证 | 保留 systemd/cgroup 监管及退出回收；真实删除后 API 返回 404、runner/VMM 消失、交换机端口释放。失败路径有定向测试，不宣称覆盖全部故障。 |
| TAP 网络接入 | 已适配基础路径 | 识别持久 TAP，在正确 netns 内由 Firecracker 按接口名打开；已验证创建和访问闭环，Guest Internet 出口未验证。 |
| 静态 CPU / 内存配置 | 已适配、已验证 | 本地测试为 1 CPU、384 MiB；完整 CPU 限流效果及生产容量未验证。 |
| 动态内存 / balloon | 未适配，技术上可行 | Firecracker 有 virtio-balloon；需增加后端中立 actuator/observer，接入 guest demand、节点 reservation 和 cgroup 控制，不能只取消静态配置限制。 |
| 快照 / 恢复、暂停 / 恢复 | 未适配，需独立设计 | Firecracker 本身提供相关原语，但 VMM 状态、内存、磁盘、vsock 和 runtime 绑定需一起适配；不能复用 CH 快照格式。 |
| Reset、导入 / 导出、迁移 | 当前拒绝 | 依赖可恢复的后端身份、artifact 及可写磁盘语义；当前 raw 工作盘是一次性的，不是导出或恢复来源。 |
| Builder | 当前拒绝 | Builder 有独立启动及 artifact 生产流程；需明确构建后端和产物兼容性，不能假设普通 Create 适配后即可使用。 |
| Workspace、动态磁盘槽、显式持久 DIFF | 当前不支持 | 需补齐挂载、设备变更、写回和数据所有权契约；不能把当前磁盘物化路径当作持久存储实现。 |
| 手动 DISCARD、本地加密存储 | 当前不支持 | 需分别设计 raw 磁盘回收和加密边界；不能因原 CH 路径支持就直接放开。 |
| E2B / Envd | 未验证 | 保留了共享链路，但本轮只验证 bare profile；需要适配模板并验证 `/init`、命令和文件等 E2B 服务。 |
| x86_64、生产性能与密度 | 未验证 | 已更新相关内核预设，但未重建并验证这些预设；ARM64 TCG 功能测试不能证明原生 KVM 性能。 |

### 3.1 动态内存的评估结论

Cloud Hypervisor 和 Firecracker 的传统 balloon 都依赖 Guest virtio-balloon 驱动。区别主要在管理 API、状态观测和当前 Kuasar 集成程度，而不是完全不同的 balloon 原理。

- CH 路径已经集成 balloon、Guest 内存报告、cgroup 和可选节点 reservation 协调。
- 当前 Firecracker 冷启动路径没有创建 balloon 设备，也没有接入该控制闭环。
- Firecracker API 可以设置 balloon target 并读取 actual pages；**target 更新成功不表示回收完成**。
- 节点增加预算时应先取得 reservation 再向 Guest 归还内存；缩减预算应先确认实际回收，再释放 reservation。
- Guest 统计可能滞后；需要明确新鲜度和异常处理，不能把每次 HTTP 成功读取都当作新采样。
- `deflate_on_oom` 可能在没有节点授权的情况下增加 Guest 可用预算，需要保守记账和宿主机余量。
- `memory.high` 是软压力控制，不是硬隔离；balloon 和 Guest 上报也不能作为不可信 Guest 的唯一安全边界。
- 节点压力回收当前可能调用暂停/快照，Firecracker 尚不支持这条生命周期，需要显式排除或另行定义策略。

建议从冷启动 balloon 开始：初始按完整 capacity 准入，验证回收后再缩减 reservation；暂不同时引入 snapshot/restore 或 memory hotplug。本阶段只做了可行性分析，**未实现动态内存代码，也未进行动态伸缩实测**。

## 4. 适配改动：按仓库与组件划分

| 仓库 / 组件 | 本次主要改动 | 目的与边界 |
| --- | --- | --- |
| orchestrator / 配置 | 增加 `sandbox.backend`、`sandbox.firecracker_binary`，校验静态资源及动态控制限制 | 默认仍为 CH；先建立受控的节点级入口。 |
| orchestrator / Conductor | 持久化 backend，生成可信启动参数，添加能力拒绝与恢复检查 | 复用状态机及鉴权，不另建 Sandbox Management 服务。 |
| orchestrator / runner | 校验 backend 参数并保持 cgroup/readiness FD 交接 | backend 由可信规格确定，不能通过不受控参数替换。 |
| orchestrator / 生命周期 | 拒绝 Firecracker 尚未实现的 Build、快照、Reset、Workspace、迁移等路径 | 在允许范围内失败关闭，不偷偷回落 CH。 |
| sandboxer / CLI、SDK | 增加 backend 选择和 Firecracker binary 参数，分流运行路径 | 调用入口仍为 `sandbox-ctl run`，不是把 Firecracker 二进制重命名为 sandbox-ctl。 |
| sandboxer / VMM 启动 | 实现 Firecracker Unix HTTP API 配置 machine、boot、disk、network、vsock 及启动动作 | 保留 cgroup/netns 放置、子进程退出与清理职责。 |
| sandboxer / 存储 | 将逻辑 block view 物化为一次性 raw virtio 工作盘；runtime bundle 作为只读 virtio 根盘 | 绕开当前 CH pmem/vhost-user 路径；有额外启动 I/O 与磁盘开销，不提供写回或快照语义。 |
| sandboxer / TAP | 添加持久 TAP 识别并释放已有 queue，让 Firecracker 按接口名接管 | Firecracker 网络 API 不直接接受现有 TAP queue FD。 |
| sandboxer / guestlink 与控制协议 | 校验版本化 Guest hello，复用 launch/exec/readiness；通过既有 unsupported 错误码报告不支持 checkpoint | 避免 Conductor 对不存在的 checkpoint 能力无限重试。 |
| guest-runtime / sandbox-init | 解析 Firecracker boot 参数、调整工作盘序号、在 Launch 挂载前清除块缓存 | Firecracker runtime 占用首个 virtio 盘；物化前的探测缓存不能污染后续挂载。 |
| guest-runtime / 内核预设 | 补充串口及 MMIO 相关选项 | 作为后续内核构建输入；不能等同于这些新预设已构建验收。 |
| 三仓测试与文档 | 增加配置、能力、恢复、参数、磁盘、TAP、Guest boot 等测试及实验记录 | 记录已支持范围和已验证证据，避免把方案当作完成状态。 |
| accelerator、connector | 获取源码用于匹配依赖；无本次适配代码修改 | Connector 网络基础能力被复用，无须为本阶段另建后端。 |

**实现涉及 orchestrator、sandboxer、guest-runtime 三个仓库，需要配套保存和构建。** 仅提交或重建 orchestrator 不足以得到可运行的 Firecracker 后端。

## 5. 本地验证过程与证据

### 5.1 实验环境

外层开发机是没有 `/dev/kvm` 的 x86_64 Linux VM。通过 ARM64 QEMU TCG 模拟虚拟化扩展，在 Debian worker 内获得 KVM，再启动实际 microVM：

```text
x86_64 开发 VM（外层无 /dev/kvm）
	→ QEMU ARM64 TCG（virtualization=on）
		→ Debian ARM64 worker（内部 /dev/kvm 可用）
			→ Conductor / runner / sandbox-ctl
				→ Cloud Hypervisor 或 Firecracker
					→ ARM64 Sandbox
```

| 项目 | 实测配置 |
| --- | --- |
| QEMU | Ubuntu 包 6.2，ARM64 TCG，`cortex-a72`，GICv3 |
| Debian worker | Debian 13 ARM64，内核 `6.12.116+deb13-arm64`，4 vCPU、4500 MiB |
| 基线产品 | Kuasar release-v0.1.13；后续替换为修改源码交叉构建的组件 |
| Firecracker | v1.18.0 |
| Cloud Hypervisor | 基线发行包 v51.1.0 |
| Sandbox | bare Alpine 3.22.8 ARM64，1 CPU、384 MiB，192 MiB ext4 overlay 模板 |
| runner pool | 按需启动，容量 1 |

worker 内实际执行 KVM ioctl，得到 `KVM_GET_API_VERSION=12` 和 `KVM_CREATE_VM=OK`，而不是仅检查设备节点存在。

### 5.2 验证步骤

1. 先跑通发行包 CH 基线，确认环境、网络及模板可用。
2. 交叉构建修改后的 `node-ctl`、`sandbox-ctl`、Guest init，重新打包带 digest 的 runtime bundle。
3. 在独立路径安装适配版本，保留原始发行包，以便对照和回退。
4. 使用 Conductor API 创建 Firecracker Sandbox，等待真实 `running`。
5. 经 Proxy 鉴权执行命令，验证 `hello-from-kuasar`、`Linux aarch64` 和文件读写结果 `writable-ok`。
6. 删除 Sandbox，等待异步清理完成：DELETE 返回 204，随后 GET 返回 404。
7. 检查 runner/VMM 进程消失，交换机恢复 64 个可用端口。
8. 使用修改后的全部组件切回 CH，再执行相同生命周期回归。

结果：**Firecracker 完整闭环成功 3 次；修改后 CH 闭环成功 1 次。** 配置、Conductor、runner、FD 交接、sandbox CLI/SDK、guestlink、TAP 和 Guest init 等定向 Go 测试通过；backend 定向 race 测试通过。不是全量 E2E 或发布认证。

### 5.3 实测暴露并修复的问题

| 问题 | 修复 |
| --- | --- |
| Firecracker 冷启动仍携带独立 `--run-id`，触发 sandbox-ctl 的 checkpoint retention 参数约束 | 将 `--run-id` 与 `--retain-instance`、`--enable-checkpoint` 一起保留在 CH 路径。 |
| Firecracker 返回普通 checkpoint 错误，Conductor 将其当作可重试维护失败 | 改为返回既有 `ErrCheckpointUnsupported` 语义。 |
| 验证脚本假定 DELETE 后立刻 GET 404 | 改为有截止时间的轮询，匹配异步清理契约。 |

实验服务使用 transient systemd unit，**不是重启自动恢复的生产部署**；Git 不会保存 VM 磁盘、密钥、下载工件及实验状态。最后一次验证结束时已删除测试 Sandbox，实验基础服务保留运行；此处记录不代表后续实时状态。

## 6. 后续阶段：已讨论但尚未实现

### 6.1 创建请求级选择

目标语义是“**创建时可选，创建后不可变**”：

- Create 显式指定已注册 backend，未指定时采用节点默认值。
- 接受时保存解析结果，后续 exec/delete/pause 等操作使用保存的 backend。
- 改变默认值只影响新 Sandbox，不能改变已有 VM。
- 同一个 Conductor 可以管理不同 backend 的 Sandbox。
- 资源、kernel/runtime、模板兼容、生命周期能力和恢复检查必须按每个 Sandbox 判断，替换当前节点级限制。

### 6.2 可插拔后端

建议将共享状态机和具体 VMM 实现拆开，先提供编译期接口及 backend registry，再按需要评估外部可执行插件。

backend 契约至少覆盖：能力声明、配置校验、启动与就绪、进程生命周期、清理，以及可选的快照/内存控制接口。Conductor 应按能力判断是否支持操作，而不是不断增加 backend 名称分支。

插件必须由管理员可信安装；请求只能选择注册名称，不能传入任意可执行文件路径、启动参数或 FD。若支持独立安装而不重编译，还需版本化协议、兼容性检查和监管契约。**当前实现还不是插件框架。**

## 7. 相关记录

- [Firecracker 适配范围与复测说明](docs/firecracker.md)
- [QEMU / Debian 本地实验记录](docs/qemu-lab.md)
- [现有 runtime 边界分析](runtime_adapter_zh.md)
- [节点资源模型](docs/node-resource_zh.md)