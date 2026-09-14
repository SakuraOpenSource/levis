# Levis

Levis is a lightweight, streamlined business management system.

## Features

- [ ] Site homepage
- [x] Store
  - [x] List view
  - [x] Grouped view
- [x] Shopping cart
- [x] User center
  - [x] Homepage
  - [x] Business management
    - [x] Purchased products
      - [ ] Product groups
- [x] Finance
  - [x] Wallet
  - [x] Billing
- [x] Support
  - [x] Ticket system
- [x] Security center
  - [x] Identity verification
    - [ ] External API
  - [x] Account security settings
  - [x] API key management
- [x] Admin panel

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
  server/             route assembly
  web/                go:embed + SPA fallback
```

### Authentication & security

- Login issues two cookies: `levis_token` (JWT HS256, httpOnly, SameSite=Lax) and `levis_csrf` (readable by JS)
- CSRF double-submit: the frontend interceptor copies `levis_csrf` into `X-CSRF-Token`, and the middleware compares it on every non-GET/HEAD/OPTIONS request. GET requests seed the token — without that, new visitors could not get past the installer page
- Registration always creates `role=user` through a dedicated DTO; client-supplied `role` / `balance_cents` are always ignored
- Passwords use bcrypt cost 12; `config.json` has permission `0600`

## API

Common prefix `/api`. Success returns the payload directly; failure returns `{"code":"...","message":"..."}` with the matching status code. Pagination uses `?page=&page_size=` (default 20, max 100) and returns `{items,total,page,page_size}`.

**Public**

```
GET  /bootstrap                 installation status, basic site info
POST /install/test-db           test database connection
POST /install                   run installation
POST /auth/register|login|logout
GET  /catalog/categories        two-level groups (with nested products)
GET  /catalog/products?category_id=
GET  /catalog/products/:id
```

**Authenticated**

```
GET   /me                       PATCH /me/email    POST /me/password
GET   /cart/items               POST /cart/items
PATCH /cart/items/:id           DELETE /cart/items/:id
POST  /orders                   check out the cart
GET   /orders                   GET /orders/:id
POST  /orders/:id/pay           finance flow (pay => provision service => generate bill)
POST  /orders/:id/cancel
GET   /services                 GET /services/:id
GET   /wallet                   GET /wallet/transactions   POST /wallet/recharge
GET   /invoices                 GET /invoices/:id
```

**Admin only**

```
GET|POST /admin/users           PATCH|DELETE /admin/users/:id
GET|POST /admin/categories      PATCH|DELETE /admin/categories/:id
GET|POST /admin/products        PATCH|DELETE /admin/products/:id
GET      /admin/stats
```

`balance_cents` in `PATCH /admin/users/:id` is the **target balance**.

`POST /wallet/recharge` and `POST /orders/:id/pay` are currently simulated recharges; real payment gateways will be integrated later.

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
