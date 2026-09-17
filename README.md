# Levis

Levis is a lightweight business management system for hosting providers: a storefront, billing, service provisioning, and an admin panel in one single-binary deployment. It ships as a pure-Go binary with an embedded web UI and supports SQLite, MySQL, and PostgreSQL.

## Features

- Storefront with categories, product groups, regions, and flexible/fixed spec configuration
- Shopping cart, orders, invoices, and a unified checkout that mixes wallet balance with third-party payment
- Payment plugins (Epay and more) with per-payment-method configuration
- Service lifecycle: automatic provisioning through upstream plugins (Virtualis, ZJMF), retry on failure
- User-facing service management: reinstall, NAT port mappings, live metrics charts, VNC console
- Traffic billing with per-product elastic pricing, fixed prices, and a site-wide fallback price
- Auth: sessions with CSRF double-submit, optional e-mail verification codes for registration and login (built-in SMTP client, no plugin required)
- Support: ticket system with attachments, knowledge base articles
- KYC: manual review or external KYC plugins
- Agent program: reseller applications, balance tiers, group discounts
- Admin panel: users, finance, orders, invoices, services, plugins, site/homepage settings
- Plugin system: out-of-process gRPC plugins for payments, provisioning, KYC, mail, and NAT management

## Quick start

Download the binary for your platform from [Releases](https://github.com/SakuraOpenSource/levis/releases), then:

```bash
chmod +x levis-os-arch
./levis-os-arch
```

Open <http://localhost:8080> in your browser to run the installer.

### CLI flags

| Flag | Default | Description |
|---|---|---|
| `-data` | `data` | Data directory holding `config.json` and the local database file |
| `-listen` | Value from the config file (initially `:8080`) | Listen address, overrides the config |
| `-debug` | `false` | Debug mode (prints routes and request logs) |
| `-version` | | Print the version and exit |

### Data directory

After installation, `data/` contains:

- `config.json` — configuration file
- `levis.db` — SQLite database (SQLite mode only)
- `plugins/` — installed plugin binaries and data

Deleting `config.json` returns the program to the uninstalled state (data in the database is kept).

## Building from source

### Requirements

- Go 1.26+
- Node 24+
- pnpm 11+

### Repository layout

```
Levis-Project/
├── levis/            # this repository
└── levis-frontend/
```

### Build

```bash
git clone https://github.com/SakuraOpenSource/levis.git
git clone https://github.com/SakuraOpenSource/levis-frontend.git
cd levis
make build        # build frontend => copy into internal/web/dist => compile binary
./bin/levis
```

If the frontend checkout lives somewhere else, use `make build FRONTEND=/path/to/levis-frontend`.

| Target | Description |
|---|---|
| `make build` | Frontend + backend, produces `bin/levis` |
| `make backend` | Backend only, reuses the existing `internal/web/dist` assets |
| `make frontend` | Builds the frontend and copies it into `internal/web/dist` |
| `make release` | Cross-compiles multi-platform binaries into `bin/release` |
| `make test` / `make vet` / `make fmt` | Tests, static checks, formatting |
| `make dev-backend` / `make dev-frontend` | Start the backend (`:8080`) and the Vite dev server (`:5173`, proxies `/api`) respectively |

The backend can run without a frontend build, but it reports: frontend not built.

## Development

```bash
make dev-backend     # terminal 1
make dev-frontend    # terminal 2, visit http://localhost:5173
```

## Architecture

### Directory layout

```
cmd/levis/            entrypoint: flag parsing, config loading, HTTP server startup
internal/
  config/             config.json read/write, DSN assembly
  database/           three-driver Open + AutoMigrate
  runtime/            runtime container (hot-swapped after installation)
  model/              GORM models
  service/            business logic
  handler/            HTTP handlers (thin layer: binding and responses only)
  middleware/         auth / admin / installed / csrf / recover / logger
  httpx/              request/response helpers (leaf package, avoids handler <-> middleware import cycles)
  mailer/             built-in SMTP client for verification e-mails
  plugin/             out-of-process plugin host (gRPC)
  server/             route assembly
  web/                go:embed + SPA fallback
```

### Authentication & security

- Login issues two cookies: `levis_token` (JWT HS256, httpOnly, SameSite=Lax) and `levis_csrf` (readable by JS)
- CSRF double-submit: the frontend interceptor copies `levis_csrf` into `X-CSRF-Token`, and the middleware compares it on every non-GET/HEAD/OPTIONS request. GET requests seed the token — without that, new visitors could not get past the installer page
- Optional e-mail verification codes: hashed at rest, rate-limited per mailbox, consumed on first successful check
- Registration always creates `role=user` through a dedicated DTO; client-supplied `role` / `balance_cents` are always ignored
- Passwords use bcrypt cost 12; `config.json` has permission `0600`; SMTP passwords are write-only (never echoed back)

## API

Common prefix `/api`. Success returns the payload directly; failure returns `{"code":"...","message":"..."}` with the matching status code. Pagination uses `?page=&page_size=` (default 20, max 100) and returns `{items,total,page,page_size}`.

**Public**

```
GET  /bootstrap                 installation status, site info, captcha + e-mail code switches
POST /install/test-db           test database connection
POST /install                   run installation
POST /auth/register|login|logout
POST /email/code                send a registration verification code (when enabled)
GET  /catalog/*                 categories, products
GET  /articles/*                knowledge base
```

**Authenticated**

```
GET   /me                       PATCH /me/email    POST /me/password
GET   /cart/items               POST /cart/items
PATCH /cart/items/:id           DELETE /cart/items/:id
POST  /orders                   check out the cart
GET   /orders                   GET /orders/:id
POST  /orders/:id/pay           unified checkout (wallet + third-party payment)
GET   /services                 GET /services/:id
GET   /services/:id/metrics     live CPU / memory / network metrics
GET|POST /services/:id/nat      DELETE /services/:id/nat/:mid
GET   /services/:id/vnc         WebSocket VNC console relay
GET   /wallet                   GET /wallet/transactions   POST /wallet/recharge
GET   /invoices                 GET /invoices/:id
GET   /tickets                  POST /tickets/:id/replies (multipart attachments)
```

**Admin only**

```
GET|POST /admin/users           PATCH|DELETE /admin/users/:id
GET|POST /admin/categories      PATCH|DELETE /admin/products/:id
GET|POST /admin/articles        knowledge base management
GET      /admin/stats
GET|PUT  /admin/settings/site|captcha|kyc|email|virtualis
GET      /admin/payment-plugins
POST     /admin/services/:id/retry   retry a failed provisioning
```

`balance_cents` in `PATCH /admin/users/:id` is the **target balance**.

Payments are handled by plugins (for example the Epay plugin); the wallet supports partial deduction combined with a third-party payment in one checkout.

## Tests

```bash
make test
```

## Sponsor

If you can, please support the developer, meow.

WeChat

![WeChat](https://github.com/RoyOfficial233/RoyOfficial233/blob/main/images/wechat.png?raw=true)

Alipay

![Alipay](https://github.com/RoyOfficial233/RoyOfficial233/blob/main/images/alipay.png?raw=true)

## License

This project is licensed under GPL-v3.
