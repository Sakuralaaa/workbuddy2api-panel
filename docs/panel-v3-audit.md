# panel-v3 功能对照审计

对照基线：`main@69a5e9d`（`internal/panel/index.html` + `app.js` + `panel.go` 路由）。
目标：旧面板的每个页面、按钮与接口在 panel-v3 中都有去处；没有去处的逐条说明。

结论：**旧 `/panel/api/*` 路由 41 条全部保留；旧按钮 / 输入控件 56 个中 55 个保留**（元素 id 不变，事件绑定沿用 app.js），
唯一移除的是侧边导航抽屉按钮 `navBtn`（连同壳层 `navDrawer` / `navVeil` / `navSub`），版本号移到顶栏品牌旁（`navVer`）。
旧表单字段（`name=`）50 个全部保留。旧 hash 深链继续可用：`#packages → #credits`、`#taskscenter → #tasks`、`#config → #schedule`（`VIEW_ALIAS`）。

## 1. 页面（Tab）对照

| 旧页面（main） | 新位置（panel-v3） | 说明 |
|---|---|---|
| 账号 | **账号管理** | 表格改为账号卡片；统计条、筛选、搜索、批量操作、模型锁池保留。「积分到期提醒」「官方补偿」两张卡移到积分管理 |
| 添加账号（弹窗） | 账号管理 › 「添加账号」弹窗 | 浏览器登录 + 导入两个标签；导入新增桌面端 `.info` / 登录 token JSON（`signins.go`） |
| 积分（账号页里的到期提醒 / 补偿） | **积分管理** | 每账号一行折叠：剩余 / 总额、快过期、最近到期、积分包数；展开看积分构成、逐包明细、积分变动 |
| 积分构成 | 积分管理 › 积分到期分布 / 积分构成对比 | 原「账号对比」改名「积分构成对比」；`/packages?uid=` 支持单账号按需查询 |
| 用量 | **用量管理** › 「按账号 / 模型 / 域」 | 原用量总览、用量明细、积分扣除历史原样保留；新增「按密钥」标签（默认） |
| 模型 | **模型管理** | 原「模型能力」表 + 每模型启用开关（`models.disabled`）与「测试」按钮 |
| 任务中心 | **任务管理** | 成长任务队列、查询券码、扫描 / 执行全部待办保留；新增全部账号执行、每账号最近结果与单项执行、运行历史 |
| 调度设置 | **调度管理** | 原服务 / 定时任务 / 账号池与流量治理 / 上游与高级 / 日志 全部字段保留；新增调度器状态卡、总开关、国际版每日活跃、开盲盒、`admin_key` |
| 状态与日志 | **日志管理** | 请求记录（新增按 API 密钥筛选）、运行日志（频道筛选）、积分历史 |
| （无） | **密钥管理**（新） | 多 API 密钥的创建 / 改名 / 启停 / 删除与近 7 天用量 |

## 2. 按钮与控件对照（旧 → 新）

| 旧位置 | 控件 | 新位置 |
|---|---|---|
| 顶栏 | 打开导航抽屉 (`navBtn`) | 移除：侧边抽屉改为顶栏横排导航（手机端两行顶栏 + 横向滚动），不再需要 |
| 顶栏 | 切换明暗 (`btnTheme`) | 顶栏 |
| 顶栏 | 刷新 (`btnRefresh`) | 顶栏 |
| 顶栏 | 添加账号 (`btnAdd`) | 账号管理 |
| 账号 › 积分到期提醒 | 收起 (`btnExpFold`) | 积分管理 › 积分到期提醒 |
| 账号 › 积分到期提醒 | 检查 (`btnExp`) | 积分管理 › 积分到期提醒 |
| 账号 › 官方补偿 | 检查 (`btnCompRefresh`) | 积分管理 › 官方补偿 |
| 账号 › 官方补偿 | 全部领取 (`btnCompClaimAll`) | 积分管理 › 官方补偿 |
| 账号 › 账号池 | 备注 (`btnRemarks`) | 账号管理 › 工具条「批量备注」（备注改存服务端，卡片上可单独编辑） |
| 账号 › 账号池 | 全部签到 (`btnCheckinAll`) | 账号管理 |
| 账号 › 账号池 | 旅行巡检 (`btnTravelAll`) | 账号管理 › 「全部账号执行」菜单；任务管理 › 全部账号执行 › 猫猫旅行 |
| 账号 › 账号池 | 活跃上报 (`btnActivityAll`) | 账号管理 › 「全部账号执行」菜单；任务管理 › 全部账号执行 |
| 账号 › 账号池 | 全部保活 (`btnKeepaliveAll`) | 账号管理 › 「全部账号执行」菜单；任务管理 › 全部账号执行 › Token 保活 |
| 任务中心 › 成长任务队列 | 查询券码 (`btnSchoolVouchers`) | 任务管理 › 成长任务队列 |
| 任务中心 › 成长任务队列 |  (`qcConc`) | 任务管理 › 成长任务队列 |
| 任务中心 › 成长任务队列 | 扫描待办 (`btnScanAll`) | 任务管理 › 成长任务队列 |
| 任务中心 › 成长任务队列 | 执行全部待办 (`btnRunQueue`) | 任务管理 › 成长任务队列 |
| 模型 › 模型能力 | 重新获取 (`btnModels`) | 模型管理 › 模型管理 |
| 模型 › 模型能力 | 搜索模型 ID / 名称 / 描述 / 厂商 (`mdQ`) | 模型管理 › 模型管理 |
| 模型 › 模型能力 | 按域筛选 (`mdRealm`) | 模型管理 › 模型管理 |
| 模型 › 模型能力 | 按能力筛选 (`mdCap`) | 模型管理 › 模型管理 |
| 模型 › 模型能力 | 按思考档位筛选 (`mdEffort`) | 模型管理 › 模型管理 |
| 模型 › 模型能力 | 按价格筛选 (`mdPromo`) | 模型管理 › 模型管理 |
| 模型 › 模型能力 | 排序方式 (`mdSort`) | 模型管理 › 模型管理 |
| 模型 › 模型能力 | 重置 (`mdReset`) | 模型管理 › 模型管理 |
| 积分构成 › 账号对比 | 逐包明细的排序规则 (`pkSort`) | 积分管理 › 积分构成对比 |
| 积分构成 › 账号对比 | 刷新 (`btnPk`) | 积分管理 › 积分构成对比 |
| 用量 › 用量总览 | 刷新 (`btnUsage`) | 用量管理 › 用量总览 |
| 用量 › 用量明细 |  (`usSort`) | 用量管理 › 用量明细 |
| 调度设置 › 服务 | 留空 = 不鉴权 (`cfgKey`) | 调度管理 › 服务与密钥 |
| 调度设置 › 服务 | 显示 (`btnEye`) | 调度管理 › 服务与密钥 |
| 调度设置 | 放弃修改 (`btnCfgReload`) | 调度管理 › 底部吸附保存条 |
| 调度设置 | 保存配置 (`btnCfgSave`) | 调度管理 › 底部吸附保存条 |
| 状态与日志 › 请求记录 | 搜索 IP / UA / 模型 / 账号 / 请 (`reqQ`) | 日志管理 › 请求记录 |
| 状态与日志 › 请求记录 | 按结果筛选 (`reqOutcome`) | 日志管理 › 请求记录 |
| 状态与日志 › 请求记录 | 读取条数 (`reqLimit`) | 日志管理 › 请求记录 |
| 状态与日志 › 请求记录 | 重新读取 (`btnReqReload`) | 日志管理 › 请求记录 |
| 状态与日志 › 运行日志 | 自动滚动：开 (`btnLogPin`) | 日志管理 › 运行日志 |
| 状态与日志 › 积分历史 | 读取条数 (`creditLimit`) | 日志管理 › 积分历史 |
| 状态与日志 › 积分历史 | 重新读取 (`btnCreditReload`) | 日志管理 › 积分历史 |
| 弹窗 › 积分任务 | 全部接受 (`btnTaskAcceptAll`) | 弹窗 › 积分任务 |
| 弹窗 › 积分任务 | 一键完成可自动任务 (`btnTaskAutoAll`) | 弹窗 › 积分任务 |
| 弹窗 › 积分任务 | 重新查询 (`btnTaskReload`) | 弹窗 › 积分任务 |
| 弹窗 › 积分任务 | 关闭 (`btnCloseTask`) | 弹窗 › 积分任务 |
| 弹窗 › 开学季 · 我的券码 | 刷新 (`btnVcRefresh`) | 弹窗 › 开学季 · 我的券码 |
| 弹窗 › 开学季 · 我的券码 | 关闭 (`btnVcClose`) | 弹窗 › 开学季 · 我的券码 |
| 弹窗 › 添加账号 |  (`importFile`) | 账号管理 › 添加账号 |
| 弹窗 › 添加账号 | 获取授权链接 (`btnStartLogin`) | 账号管理 › 添加账号 |
| 弹窗 › 添加账号 | 复制链接 (`btnCopyUrl`) | 账号管理 › 添加账号 |
| 弹窗 › 添加账号 | 在浏览器打开 (`btnOpenUrl`) | 账号管理 › 添加账号 |
| 弹窗 › 添加账号 | 关闭 (`btnCloseAdd`) | 账号管理 › 添加账号 |
| 弹窗 › 需要访问密钥 | 密钥输入 (`keyInput`) | 登录封面（全屏，服务端校验 admin_key） |
| 弹窗 › 需要访问密钥 | 进入 (`btnKey`) | 登录封面 › 进入面板 |
| 弹窗 › 账号备注 | 清空全部 (`btnRemarkClear`) | 弹窗 › 账号备注 |
| 弹窗 › 账号备注 | 关闭 (`btnRemarkClose`) | 弹窗 › 账号备注 |
| 弹窗 › 账号备注 | 保存 (`btnRemarkSave`) | 弹窗 › 账号备注 |

账号卡片动作（`data-a`）：`checkin` 签到、`balance` 余额 / 企业额度、`tasks` 积分任务、`pause` 暂停选号、`resume` 恢复选号、
`revive` 解冻、`disable` 禁用、`remove` 删除——新旧完全一致（直出两个 + 「…」更多菜单），事件委托不变。
新增表单字段：`admin_key`、`schedule_enabled`、`travel_open_boxes`、`intl_daily_enabled/hours/web/models/prompt`、`blackcat_enabled` / `blackcat_hours`（夜猫子此前只能改配置文件）。

## 3. 接口对照

旧路由（`panel.go`）全部原样保留，前端调用位置：

| 接口（`/panel/api/…`） | 新前端调用处 |
|---|---|
| `GET overview` | 账号管理 / 积分管理 / 任务管理（5s 轮询） |
| `GET logs` · `GET request_metrics` · `GET request_logs` · `GET credit_history` | 日志管理；`request_logs?api_key=` 新增密钥筛选；`credit_history?uid=` 也用于积分管理行展开 |
| `GET models` · `GET model_probes` | 模型管理 |
| `POST login/start` · `GET login/poll` | 添加账号 › 浏览器登录 |
| `GET login/regions` | 前端未调用（旧版同样未调用），保留给外部脚本 |
| `POST import/cockpit` | 添加账号 › 导入（cockpit 数组 / `.info` / token JSON 自动识别） |
| `POST accounts/{uid}/revive·disable·pause·resume·checkin·balance·remove` | 账号卡片动作；`checkin`/`balance` 也在积分管理行内 |
| `GET accounts/{uid}/tasks` · `POST …/tasks/accept·accept_all·auto·auto_all` | 积分任务弹窗；任务管理行展开 |
| `POST accounts/{uid}/tasks/claim` | 前端未调用（旧版同样未调用；领奖走 accept/auto 链路与补领清扫），保留 |
| `POST tasks/scan_all` · `POST tasks/run_queue` · `GET tasks/queue` · `GET school/vouchers` | 任务管理 › 成长任务队列 |
| `POST checkin_all·travel_all·activity_all·keepalive_all` | 账号管理工具条（全部签到 / 全部账号执行菜单） |
| `POST balance_all` | 前端改用 `POST jobs/balance/run_all`（积分管理「刷新全部余额」，带运行历史）；旧接口保留 |
| `GET packages` | 积分管理（到期提醒、构成对比、行展开 `?uid=`） |
| `GET usage` | 用量管理 › 按账号 / 模型 / 域 |
| `POST usage/save` | 前端未调用（旧版同样未调用），保留 |
| `GET/POST config` | 调度管理 |
| `GET compensation` · `POST accounts/{uid}/compensation` · `POST compensation_all` | 积分管理 › 官方补偿 |

panel-v3 新增路由（`v3.go`）：`GET auth/info`、`GET auth/check`、`GET/POST keys`、`POST keys/{id}/rename·enabled·delete`、
`GET keys/usage`、`POST models/toggle`、`POST models/test`、`GET notes`、`POST notes/import`、`POST accounts/{uid}/note`、
`GET task_records`、`GET runs`、`GET runs/{id}`、`POST runs/clear`、`POST accounts/{uid}/run`、`POST jobs/{job}/run_all`、`GET schedule/status`，以及静态 `GET /panel/v3.js`。

## 4. 本轮审计中修复的问题

- 主样式表多出一个 `}`，把全局 `* { box-sizing: border-box }` 规则吞掉 → 桌面端整页横向溢出、输入框溢出卡片。已删除。
- 深链直接打开 `#usage` / `#schedule` / `#keys` / `#tasks` 时页面空白：首次路由用 `setTimeout(0)`，早于 `v3.js` 求值。改为 `DOMContentLoaded` 后路由。
- 统计条「积分剩余 / 总额」把企业版不限额（`-1`）算进总额，且无千分位。
- 积分 / 任务 / 用量 / 运行历史折叠行各列随行内容变宽而上下错位 → 固定列宽。
- 官方补偿查询失败时把上游整段 HTML 错误页直接显示 → 去标签只留文字（`plainErr`）。
- 调度管理「国际版每日活跃」卡片过高撑乱网格 → 改为整行卡片、字段横排；策略说明与下方字段贴在一起。
- 密钥管理说明框被 flex 拆成多列；改名铅笔图标过大；手机端密钥卡片字段错乱、顶栏状态挤压品牌名、盒子标题逐字换行。
- 用量「有调用的天数」实际是区间天数；每日请求图 SVG 被拉伸导致字号偏大。
- 积分到期分布 / 积分构成对比在未查询时是空白盒子 → 加空状态提示。
- 顶栏恢复显示版本号（旧版在侧栏底部）。
