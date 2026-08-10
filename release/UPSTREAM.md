# Upstream baseline

- Repository: https://github.com/wyx2685/v2node
- Tag: `v0.4.4`
- Commit: `2daa9dd4a114aa39294350475defa2b748d595ed`
- Original installer: https://raw.githubusercontent.com/wyx2685/v2node/master/script/install.sh
- Xray replacement pinned by upstream: `wyx2685/xray-core`
  commit `b17a88f9b46d`

The application source was reset to the exact upstream tag before applying the
RAM-only patch set. The exact pinned Xray source is included in
`third_party/xray-core`; its local delta is restricted to bounded live state,
concurrency correctness and shutdown/deadline lifecycle.

Geo assets remain frozen and are recorded by exact size/SHA-256 in
`release/manifest.json`.
