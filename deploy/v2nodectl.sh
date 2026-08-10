#!/usr/bin/env bash
set -Eeuo pipefail

current="/usr/local/v2node/current/v2node"
service="v2node.service"
installer="/usr/local/v2node/install.sh"

case "${1:-status}" in
  start)   exec systemctl start "$service" ;;
  stop)    exec systemctl stop "$service" ;;
  restart) exec systemctl restart "$service" ;;
  status)  exec systemctl status "$service" --no-pager -l ;;
  log)     exec journalctl -u "$service" -f -n 100 ;;
  version) exec "$current" version ;;
  rollback)
    [[ -x "$installer" ]] || { echo "installer not found: $installer" >&2; exit 1; }
    exec "$installer" --rollback
    ;;
  *)
    echo "Usage: v2nodectl {start|stop|restart|status|log|version|rollback}" >&2
    exit 2
    ;;
esac
