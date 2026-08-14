# Levis

轻量的云服务管理系统（后端）。前端产物嵌入在二进制里，**下载一个可执行文件、直接运行，就是完整的程序** —— 不需要 Node、不需要 Nginx、不需要预先准备数据库。

前端源码在独立仓库：[SakuraOpenSource/levis-frontend](https://github.com/SakuraOpenSource/levis-frontend)。

## 特性

- **单文件交付**：Go `embed` 打包前端，`CGO_ENABLED=0` 静态编译，跨平台无依赖
- **浏览器内安装**：首次启动无需配置文件，打开页面按引导选数据库、建管理员
- **三种数据库**：SQLite（零配置，纯 Go 驱动）、MySQL、PostgreSQL
- **完整商销链路**：商品分组（两级）→ 商店 → 购物车 → 下单 → 支付 → 开通服务 → 生成账单 → 资金流水
- **金额零误差**：一律用 `int64` 存「分」，不碰浮点
- **中文界面**：vue-i18n，中文为默认语言

## 快速开始

从 [Releases](https://github.com/SakuraOpenSource/levis/releases) 下载对应平台的产物，然后：

```bash
chmod +x levis-linux-amd64
./levis-linux-amd64
```

浏览器打开 <http://localhost:8080>，会自动跳到安装页。选 SQLite 即可一路下一步装完，装完直接以管理员身份登录。

### 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-data` | `data` | 数据目录，存放 `config.json` 与 SQLite 文件 |
| `-listen` | 配置文件中的值（初始 `:8080`） | 监听地址，覆盖配置 |
| `-debug` | `false` | 调试模式（打印路由与请求日志） |
| `-version` | | 打印版本号后退出 |

### 数据目录

安装完成后 `data/` 下会有：

- `config.json` —— 数据库连接参数、JWT 密钥、监听地址。**含敏感信息，权限为 `0600`，不要提交到仓库**
- `levis.db` —— 仅 SQLite 模式下存在

删掉 `config.json` 会让程序回到未安装态（数据库里的数据仍在）。

## 从源码构建

需要 Go 1.26+、Node 24+、pnpm 11+。两个仓库默认放成同级目录：

```
some-dir/
├── levis/            # 本仓库
└── levis-frontend/
```

```bash
git clone https://github.com/SakuraOpenSource/levis.git
git clone https://github.com/SakuraOpenSource/levis-frontend.git
cd levis
make build        # 构建前端 → 拷入 internal/web/dist → 编译二进制
./bin/levis
```

前端不在同级目录时用 `make build FRONTEND=/path/to/levis-frontend`。

| 目标 | 说明 |
|---|---|
| `make build` | 前端 + 后端，产出 `bin/levis` |
| `make backend` | 只编译后端，复用 `internal/web/dist` 中已有产物 |
| `make frontend` | 只构建前端并拷入 `internal/web/dist` |
| `make release` | 交叉编译多平台产物到 `bin/release` |
| `make test` / `make vet` / `make fmt` | 测试、静态检查、格式化 |
| `make dev-backend` / `make dev-frontend` | 分别起后端（`:8080`）与 Vite 开发服务器（`:5173`，已代理 `/api`） |

只跑 `go build` 也可以，但 `internal/web/dist` 里没有产物时访问首页会提示「前端未构建」，API 仍然可用。

## 开发

```bash
make dev-backend     # 终端 1
make dev-frontend    # 终端 2，访问 http://localhost:5173
```

Vite 把 `/api` 代理到 `127.0.0.1:8080`，浏览器视角同源，因此不需要配 CORS，cookie 也不会丢。

## 架构

### 两阶段启动

安装页的存在意味着**程序启动时数据库可能还不存在**：

1. 启动时读 `data/config.json`。不存在 → 未安装态
2. 未安装态下只有 `/api/bootstrap`、`/api/install*` 与静态资源可用，其余 `/api/*` 返回 `503 NOT_INSTALLED`
3. 前端路由守卫先请求 `/api/bootstrap`，`installed: false` 就强制跳 `/install`；已安装时访问 `/install` 反向重定向
4. `POST /api/install`：校验 → 试连数据库（失败即返错，不落盘）→ 迁移 → 建站点设置与管理员 → 生成 JWT 密钥 → 写 `config.json` → **原子替换运行时容器**。全程不重启进程
5. 已安装后再 `POST /api/install` 返回 `409`，防止二次安装覆盖

`internal/runtime.Runtime` 用 `sync.RWMutex` 持有 `*gorm.DB` 与配置，安装完成时整体换新。

### 目录结构

```
cmd/levis/            入口：flag 解析、加载配置、起 HTTP server
internal/
  config/             config.json 读写、DSN 组装
  database/           三驱动 Open + AutoMigrate
  runtime/            运行时容器（安装后热替换）
  model/              GORM 模型
  service/            业务逻辑
  handler/            HTTP handler（薄层，只做绑定与响应）
  middleware/         auth / admin / installed / csrf / recover / logger
  httpx/              请求响应工具（叶子包，避免 handler ↔ middleware 循环导入）
  server/             路由装配
  web/                go:embed + SPA fallback
```

### 认证与安全

- 登录下发两个 cookie：`levis_token`（JWT HS256，httpOnly、SameSite=Lax）与 `levis_csrf`（可被 JS 读取）
- CSRF 双提交：前端拦截器把 `levis_csrf` 复制到 `X-CSRF-Token`，中间件对所有非 GET/HEAD/OPTIONS 请求比对。GET 请求负责播种 token，否则新访客连安装页都过不去
- 注册接口固定 `role=user`，用独立 DTO 接参，客户端传 `role` / `balance_cents` 一律无效
- 密码 bcrypt cost 12；`config.json` 权限 `0600`

### 金额与时间

- 所有金额是 `int64` 的**分**，字段名带 `_cents` 后缀，禁止浮点。前端只在渲染时除 100
- 余额变动一律走事务：`UPDATE users SET balance_cents = balance_cents + ?`（不读-改-写）+ 插入 `transactions`，同一个 `db.Transaction` 内完成
- 订单与账单存商品名与价格的**快照**，商品改价或下架后历史记录保持原值
- 时间统一 UTC 存储，前端按浏览器时区渲染

## API

统一前缀 `/api`。成功直接返数据，失败返 `{"code":"...","message":"..."}` + 对应状态码。分页用 `?page=&page_size=`（默认 20，最大 100），返回 `{items,total,page,page_size}`。

**公开**

```
GET  /bootstrap                 是否已安装、站点名称与简介
POST /install/test-db           试连数据库，不落盘
POST /install                   执行安装
POST /auth/register|login|logout
GET  /catalog/categories        两级分组（含嵌套商品）
GET  /catalog/products?category_id=
GET  /catalog/products/:id
```

**需登录**

```
GET   /me                       PATCH /me/email    POST /me/password
GET   /cart/items               POST /cart/items
PATCH /cart/items/:id           DELETE /cart/items/:id
POST  /orders                   购物车结账下单
GET   /orders                   GET /orders/:id
POST  /orders/:id/pay           扣余额 → 开通服务 → 生成账单 → 记流水（单事务）
POST  /orders/:id/cancel
GET   /services                 GET /services/:id
GET   /wallet                   GET /wallet/transactions   POST /wallet/recharge
GET   /invoices                 GET /invoices/:id
```

**需管理员**

```
GET|POST /admin/users           PATCH|DELETE /admin/users/:id
GET|POST /admin/categories      PATCH|DELETE /admin/categories/:id
GET|POST /admin/products        PATCH|DELETE /admin/products/:id
GET      /admin/stats
```

`PATCH /admin/users/:id` 的 `balance_cents` 是**目标余额**，后端算差额并记为 `adjust` 流水。字段全为指针，未提交的字段不改动。管理员不能给自己降权、禁用或删除，也不能删掉最后一个管理员。

`POST /wallet/recharge` 与 `POST /orders/:id/pay` 目前都是模拟实现（直接改余额），接口形状保持不变，后续接真实支付渠道只需替换实现。

商品的 `specs` 是展示用的规格列表（`[{"label":"CPU","value":"4 核"}]`），以 JSON 文本存在单列里，三种数据库共用一份定义。写入时空白行会被丢弃，只填一半的行返 400，单条上限 32/64 字符、总数上限 20 条。加字段前的历史行读出来是空列表。

## 技术约束

**SQLite 必须用纯 Go 驱动。** 项目用 `github.com/glebarez/sqlite`（底层 `modernc.org/sqlite`），不能换成官方 `gorm.io/driver/sqlite` —— 后者依赖 cgo 的 `mattn/go-sqlite3`，`CGO_ENABLED=0` 直接编译失败，交叉编译还要配 C 工具链，单文件交付就没了。

附带注意：`glebarez/sqlite` 的 `DriverName` 是 `sqlite` 而非 `sqlite3`；pragma 走 DSN 查询参数（`?_pragma=foreign_keys(1)`）。

## 测试

```bash
make test
```

单测覆盖三处最容易出错的地方：资金事务（余额不足必须整体回滚，`balance_after_cents` 必须与最终余额一致）、权限（注册不得提权、未安装态返 503、二次安装返 409）与规格列的 JSON 往返。

## License

见 [LICENSE](LICENSE)。
