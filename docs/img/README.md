# README screenshots — how to regenerate

The dashboard screenshots in the project README are captured at **2560px wide, dark theme,
device-pixel-ratio 1**, then cropped down to the footer so each ends just below
`Heliograph <version>`. Regenerate them from the **released tree** whenever the dashboard UI
changes materially, so the README always shows the current default behaviour.

| File | Tab | Source |
|------|-----|--------|
| `dashboard-overview-dark.png` | Overview | any running collector with demo data |
| `dashboard-graphs-dark.png` | Graphs → All targets | same |
| `federation-overlay-dark.png` | Graphs → one target, zoomed | a hub with ≥2 live vantages (below) |
| `config-tree-dark.png` | Config (admin) | a federated hub, or a seeded local instance (below) |
| `vantages-dark.png` | Vantages (admin) | same |
| `smoke-graph-dark.png` | — | static demo from `web/smoke-poc.html` (regenerate only if that page changes) |

The current set was captured from the live federated hub (`heliograph.bsd-unix.net`) so the graphs,
the vantage overlay, and the Vantages table all show real multi-vantage data. The seeded-instance
recipe below stays the credential-free fallback for the Config/Vantages panels when no federated hub
is at hand — but the federation overlay itself needs a hub with vantages actually reporting.

## Overview + Graphs

These need only live demo data, so the public site works:

1. Browser viewport `2560×1300`, dark theme (`data-theme=dark`).
2. Load the Overview (`/`) and the Graphs grid (`/#graphs`, "All targets"); wait for panels to paint.
3. Full-page screenshot, then crop to the footer's bottom:
   `magick raw.png -crop 2560x<footerBottom+~25>+0+0 +repage docs/img/<name>.png`

## Federation overlay

Needs a hub with **≥2 vantages actively reporting** (so each has real data to overlay); the public
federated site works. On the Graphs page, turn every vantage on in the **Vantages** control (cycle
each to `band`/`line`), click one target that all vantages measure (an external host such as
`Servers/github.com`, whose ISP paths visibly differ), then click a graph to zoom. If a single
per-round spike blows out the y-axis and compresses the lines together, drag-select a spike-free
window on the canvas — the y-axis rescales and the per-vantage medians separate into distinct bands.
Screenshot the viewport and crop to the footer as above.

## Config + Vantages (admin)

Capture these from any admin session — the live federated hub shows the real vantages; otherwise
seed a throwaway instance:

The Config tree and Vantages table require the admin API, a base `-config`, and DB-backed
targets, so seed a throwaway instance:

```sh
# 1. throwaway TimescaleDB
docker run -d --name heli-shot-db -e POSTGRES_DB=smoke -e POSTGRES_USER=smoke \
  -e POSTGRES_PASSWORD=smoke -p 15499:5432 timescale/timescaledb:2.29.1-pg16
DSN="postgres://smoke:smoke@localhost:15499/smoke?sslmode=disable"

# 2. import the demo targets as a DB fragment (targets: -> children: only; no globals),
#    and mint a few vantages for the Vantages table
#    (the fragment is config.example.yaml's `targets:` subtree, rooted at `children:`)
smoked config import demo-children.yaml -dsn "$DSN"
for v in fra-edge lon-edge nyc-edge; do smoked vantage add "$v" -dsn "$DSN"; done

# 3. serve with a GLOBALS-ONLY base config (database/probes/alerts + empty `targets.children`),
#    so /api/admin/config is enabled and the DB fragment is the editable tree.
#    Build with the version ldflag so the footer reads the real version, not "dev".
go build -ldflags "-X main.version=vX.Y.Z" -o smoked ./cmd/smoked
SMOKED_ADMIN_PASSWORD=demo1234 ./smoked -serve -addr :18099 -webdir web \
  -config base-globals.yaml -dsn "$DSN" -resolve-ips
```

Then in the browser (fresh context so `/api/version` isn't cached from an earlier run):
`POST /api/admin/login {"password":"demo1234"}`, reload `/#config` and `/#vantages`, confirm the
footer reads the version, screenshot, crop as above. Finally `docker rm -f heli-shot-db`.

**Note:** GitHub renders every README image at the ~838px content-column width regardless of
intrinsic size, so 2560px only keeps them crisp — it does not make them render larger.
