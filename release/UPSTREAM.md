# Upstream baseline

- Repository: https://github.com/wyx2685/v2node
- Tag: `v0.4.4`
- Commit: `2daa9dd4a114aa39294350475defa2b748d595ed`
- Requested upstream installer: https://raw.githubusercontent.com/wyx2685/v2node/master/script/install.sh

The personal fork does not execute the upstream floating installer. Its source patch is kept in `source/`, and `deploy/install.sh` consumes only an explicit package plus SHA-256.

Geo assets are frozen in this bundle and recorded by exact size/SHA-256 in `manifest.json`; builds do not download a floating geodata branch.
