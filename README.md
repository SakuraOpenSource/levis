# Levis

Levis 是一个轻量、简洁的业务管理系统。

## 特性

- [ ] 站点主页
- [x] 商店
 - [x] 列表视图
 - [x] 分组视图
- [x] 购物车
- [x] 用户中心
 - [x] 主页
 - [x] 业务管理
  - [x] 已购买的产品
   - [ ] 产品分组
 - [x] 财务
  - [x] 钱包
  - [x] 账单
 - [x] 安全中心
  - [ ] 实名认证
  - [x] 账户安全设置
- [x] 管理后台

## 快速开始

从 [Releases](https://github.com/SakuraOpenSource/levis/releases) 下载对应平台的产物，然后：

```bash
chmod +x levis-linux-amd64
./levis-linux-amd64
```

浏览器打开 <http://localhost:8080> 即可进行安装。

### 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-data` | `data` | 数据目录，存放 `config.json` 与本地数据库文件 |
| `-listen` | 配置文件中的值（初始 `:8080`） | 监听地址，覆盖配置 |
| `-debug` | `false` | 调试模式（打印路由与请求日志） |
| `-version` | | 打印版本号后退出 |

### 数据目录

安装完成后将自动在 `data/` 生成以下文件：

- `config.json` —— 配置文件
- `levis.db` —— SQLite 数据库（仅 SQLite 模式下存在）

删掉 `config.json` 会让程序回到未安装状态（数据库里的数据仍在）。

## 从源码构建

### 环境要求
- Go 1.26+
- Node 24+
- pnpm 11+

### 仓库结构

```
Levis-Project/
├── levis/            # 本仓库
└── levis-frontend/
```

### 构建流程

```bash
git clone https://github.com/SakuraOpenSource/levis.git
git clone https://github.com/SakuraOpenSource/levis-frontend.git
cd levis
make build        # 构建前端 => 拷入 internal/web/dist => 编译二进制
./bin/levis
```

前端不在同级目录时可使用 `make build FRONTEND=/path/to/levis-frontend`。

| 目标 | 说明 |
|---|---|
| `make build` | 前端 + 后端，产出 `bin/levis` |
| `make backend` | 只编译后端，复用 `internal/web/dist` 中已有产物 |
| `make frontend` | 只构建前端并拷入 `internal/web/dist` |
| `make release` | 交叉编译多平台产物到 `bin/release` |
| `make test` / `make vet` / `make fmt` | 测试、静态检查、格式化 |
| `make dev-backend` / `make dev-frontend` | 分别起后端（`:8080`）与 Vite 开发服务器（`:5173`，已代理 `/api`） |

后端可脱离前端运行，但会提示：前端未构建。

## 开发

```bash
make dev-backend     # 终端 1
make dev-frontend    # 终端 2，访问 http://localhost:5173
```

## 架构

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

## API

统一前缀 `/api`。成功直接返回数据，失败返会 `{"code":"...","message":"..."}` + 对应状态码。分页用 `?page=&page_size=`（默认 20，最大 100），返回 `{items,total,page,page_size}`。

**公开**

```
GET  /bootstrap                 安装状态、站点基础信息
POST /install/test-db           测试数据库连接
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
POST  /orders/:id/pay           财务处理（付款 => 开通服务 => 生成账单）
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

`PATCH /admin/users/:id` 的 `balance_cents` 是**目标余额**。

`POST /wallet/recharge` 与 `POST /orders/:id/pay` 目前为模拟充值，后续将接入真实接口。

## 测试

```bash
make test
```

## License

本项目遵循 GPL-v3 开源协议。
