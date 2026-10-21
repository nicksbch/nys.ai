#!/usr/bin/env bash
#
# homeserver-audit.sh — read-only health check for a small Ubuntu home server
#
# Checks:
#   1. Firewall  — ufw active, default-deny inbound, every ALLOW rule limited to LAN,
#                  IPv6 covered, Docker not bypassing ufw
#   2. Exposure  — what is listening on non-loopback addresses
#   3. Bloat     — desktop/laptop/cloud/telemetry services a headless home server
#                  rarely needs, failed units, memory footprint
#   4. Ours      — garage, garage-ui, llama-server, duckpipe, ducklake, vigil, relay, url-to-bucket:
#                  service running/enabled, health endpoint, secret-file permissions,
#                  nothing sensitive bound beyond loopback
#
# Usage:   sudo ./homeserver-audit.sh
#
# Options (environment variables):
#   TRUSTED_NETS="100.64.0.0/10 10.8.0.0/24"  extra non-LAN sources you accept in ufw
#                                              rules (e.g. Tailscale, WireGuard)
#   KEEP="avahi-daemon cups"                  services you actually want, so they
#                                              aren't flagged as bloat
#   e.g.  sudo KEEP="avahi-daemon" ./homeserver-audit.sh
#
# Exit code: 0 = all good, 1 = warnings only, 2 = at least one failure.
# This script changes nothing on the system.

set -u
export LC_ALL=C

TRUSTED_NETS=${TRUSTED_NETS:-}
KEEP=${KEEP:-}

PASS=0; WARN=0; FAIL=0
if [[ -t 1 ]]; then
  G=$'\e[32m'; Y=$'\e[33m'; R=$'\e[31m'; C=$'\e[36m'; B=$'\e[1m'; D=$'\e[2m'; N=$'\e[0m'
else
  G=; Y=; R=; C=; B=; D=; N=
fi

pass()    { printf '  %s[PASS]%s %s\n' "$G" "$N" "$*"; PASS=$((PASS+1)); }
warn()    { printf '  %s[WARN]%s %s\n' "$Y" "$N" "$*"; WARN=$((WARN+1)); }
fail()    { printf '  %s[FAIL]%s %s\n' "$R" "$N" "$*"; FAIL=$((FAIL+1)); }
info()    { printf '  %s[INFO]%s %s\n' "$C" "$N" "$*"; }
hint()    { printf '         %s-> %s%s\n' "$D" "$*" "$N"; }
section() { printf '\n%s== %s ==%s\n' "$B" "$*" "$N"; }

# RFC1918 / loopback / link-local / IPv6 ULA
is_private() {
  local a=${1%%/*}
  a=${a,,}
  case $a in
    10.*|192.168.*|127.*)                    return 0 ;;
    172.1[6-9].*|172.2[0-9].*|172.3[01].*)   return 0 ;;
    fe80:*|fc*|fd*|::1)                      return 0 ;;
  esac
  return 1
}

is_trusted() {
  local n
  for n in $TRUSTED_NETS; do [[ $1 == "$n" ]] && return 0; done
  return 1
}

is_kept() {
  local u=${1%.*} k
  for k in $KEEP; do [[ $u == "$k" ]] && return 0; done
  return 1
}

if [[ $EUID -ne 0 ]]; then
  echo "Please run as root: sudo $0" >&2
  exit 3
fi

# ---------------------------------------------------------------------------
printf '%sHome server audit%s - %s - %s\n' "$B" "$N" "$(hostname)" "$(date '+%Y-%m-%d %H:%M')"
# shellcheck disable=SC1091
[[ -r /etc/os-release ]] && . /etc/os-release && printf '%s\n' "${PRETTY_NAME:-}"

VIRT=$(systemd-detect-virt 2>/dev/null || true); VIRT=${VIRT:-none}
HAS_WIFI=0; compgen -G '/sys/class/net/*/wireless' >/dev/null && HAS_WIFI=1

mapfile -t LAN_NETS < <(ip -o -4 route show scope link 2>/dev/null |
  awk '$1 ~ /\// && $1 !~ /^169\.254/ && $3 !~ /^(docker|br-|veth|virbr|lxdbr|tailscale|wg)/ {print $1" ("$3")"}')
if ((${#LAN_NETS[@]})); then
  info "Detected LAN: ${LAN_NETS[*]}"
else
  info "Could not detect a LAN subnet"
fi
[[ $VIRT != none ]] && info "Running virtualised: $VIRT"

# ---------------------------------------------------------------------------
section "1. Firewall"

UFW_ACTIVE=0
UFW_RULES=""

if command -v ufw >/dev/null 2>&1; then
  st=$(ufw status verbose 2>/dev/null)
  if grep -q '^Status: active' <<<"$st"; then
    UFW_ACTIVE=1
    pass "ufw is active"
  else
    fail "ufw is installed but inactive"
    hint "add LAN rules first, then: sudo ufw enable"
  fi

  if (( UFW_ACTIVE )); then
    def=$(grep '^Default:' <<<"$st")
    re='(deny|reject) \(incoming\)'
    if [[ $def =~ $re ]]; then
      pass "Default inbound policy: ${BASH_REMATCH[1]}"
    else
      fail "Default inbound policy is not deny"
      hint "sudo ufw default deny incoming"
    fi
    if [[ $def == *"allow (routed)"* ]]; then
      warn "Forwarded (routed) traffic is allowed by default"
      hint "sudo ufw default deny routed   (unless this box is a router/VPN gateway)"
    fi

    UFW_RULES=$(awk 'f{print} /^--/{f=1}' <<<"$st" | sed '/^[[:space:]]*$/d')
    if [[ -z $UFW_RULES ]]; then
      info "No allow rules - nothing (not even SSH) is reachable from the network"
    fi

    while IFS= read -r line; do
      [[ -z $line ]] && continue
      line=${line%%#*}                                   # drop rule comments
      parsed=$(sed -E 's/^(.*[^ ]) {2,}(ALLOW|LIMIT|DENY|REJECT)( (IN|OUT|FWD))? {2,}(.*[^ ]) *$/\1|\2|\4|\5/' <<<"$line")
      IFS='|' read -r to action dir from <<<"$parsed"
      [[ $action == ALLOW || $action == LIMIT ]] || continue
      [[ -z $dir || $dir == IN ]] || continue
      src=${from%% *}

      if [[ $src == Anywhere ]]; then
        if [[ $to == *" on "* ]]; then
          warn "'$to' allowed from Anywhere (interface-restricted)"
          hint "OK only if that interface is LAN-only; tighter: sudo ufw allow from <LAN-subnet> to any port <port>"
        else
          fail "'$to' is allowed from Anywhere"
          hint "sudo ufw delete allow <rule>; sudo ufw allow from <LAN-subnet> to any port <port> proto <tcp|udp>"
        fi
      elif is_private "$src" || is_trusted "$src"; then
        pass "'$to' allowed only from $src"
      else
        fail "'$to' allowed from non-LAN address $src"
        hint "if intentional (e.g. a VPN range), add it to TRUSTED_NETS"
      fi
    done <<<"$UFW_RULES"

    v6cfg=$(grep -E '^IPV6=' /etc/default/ufw 2>/dev/null | cut -d= -f2)
    if ip -6 addr show scope global 2>/dev/null | grep -q inet6; then
      if [[ $v6cfg == yes ]]; then
        pass "Host has a public IPv6 address and ufw filters IPv6"
      else
        fail "Host has a public IPv6 address but ufw IPv6 filtering is OFF"
        hint "set IPV6=yes in /etc/default/ufw, then: sudo ufw reload"
      fi
    fi
  fi
else
  pol=$(iptables -S INPUT 2>/dev/null | head -n1)
  # Docker creates input-hook chains with "policy accept", so only a drop policy
  # counts as a real firewall
  if command -v nft >/dev/null 2>&1 && nft list ruleset 2>/dev/null | grep -Eq 'hook input .*policy (drop|reject)'; then
    warn "ufw not installed; an nftables input chain drops by default but its rules aren't checked"
    hint "review it: sudo nft list ruleset"
  elif [[ $pol == "-P INPUT DROP" ]]; then
    warn "ufw not installed; iptables INPUT policy is DROP but rules aren't checked"
    hint "review them: sudo iptables -S INPUT"
  else
    fail "No active firewall found - every listening port below is reachable from the network"
    hint "sudo apt install ufw"
    hint "sudo ufw default deny incoming && sudo ufw allow from <LAN-subnet> to any port 22 proto tcp"
    hint "sudo ufw enable"
  fi
fi

# Docker writes its own iptables rules, so published ports skip ufw entirely
if command -v docker >/dev/null 2>&1 && systemctl is-active -q docker 2>/dev/null; then
  exposed=$(docker ps --format '{{.Names}}\t{{.Ports}}' 2>/dev/null | grep -E '0\.0\.0\.0:|\[::\]:|:::')
  if [[ -n $exposed ]]; then
    fail "Docker publishes container ports on all interfaces - these bypass ufw"
    while IFS=$'\t' read -r name ports; do hint "$name: $ports"; done <<<"$exposed"
    hint "bind to the LAN IP instead (-p 192.168.1.10:8080:80) or filter in the DOCKER-USER chain"
  else
    pass "Docker is running; no container ports published on all interfaces"
  fi
fi

# ---------------------------------------------------------------------------
section "2. Listening services (non-loopback)"

count=0
while read -r proto local proc; do
  addr=${local%:*}; port=${local##*:}
  case $addr in 127.*|'[::1]'|*%lo) continue ;; esac
  name=$(sed -nE 's/.*\(\("([^"]+)".*/\1/p' <<<"$proc"); name=${name:-?}
  status=""
  if (( UFW_ACTIVE )); then
    if grep -qE "^${port}(/${proto})?[[:space:]]" <<<"$UFW_RULES"; then
      status="has a ufw rule"
    else
      status="blocked by default policy (unless an app-profile rule like 'OpenSSH' covers it)"
    fi
  fi
  printf '  %-4s %-26s %-18s %s%s%s\n' "$proto" "$local" "$name" "$D" "$status" "$N"
  count=$((count+1))
done < <(ss -H -tulnp 2>/dev/null | awk '{print $1, $5, $7}' | sort -u)
(( count == 0 )) && info "Nothing is listening on a network-facing address"

# ---------------------------------------------------------------------------
section "3. Background bloat"

# unit | why it's usually unnecessary | scope
#   scope: (empty) always check, vm = only flag on bare metal,
#          vmware = skip inside VMware, wifi = skip if a Wi-Fi interface exists
BLOAT=(
  "snapd|Snap daemon - only needed if you use snaps|"
  "cups|Print server|"
  "cups-browsed|Network printer discovery (had remote-exploit CVEs in 2024)|"
  "avahi-daemon|mDNS/Bonjour broadcasting on the LAN|"
  "ModemManager|Mobile-broadband modem support|"
  "bluetooth|Bluetooth stack|"
  "whoopsie|Uploads crash reports to Canonical|"
  "apport|Crash-report collection|"
  "kerneloops|Kernel crash reporting|"
  "packagekit|Backend for GUI software centres|"
  "udisks2|Desktop disk automounting|"
  "accounts-daemon|Desktop user-account service|"
  "colord|Display/printer colour profiles|"
  "switcheroo-control|Laptop dual-GPU switching|"
  "power-profiles-daemon|Laptop power profiles|"
  "multipathd|Multipath SAN storage|"
  "iscsid|iSCSI initiator|"
  "rpcbind|RPC portmapper (port 111) - only for NFSv3|"
  "NetworkManager|Desktop network manager (servers normally use netplan + systemd-networkd)|"
  "wpa_supplicant|Wi-Fi authentication on a wired-only machine|wifi"
  "gdm|GNOME display manager|"
  "lightdm|Display manager|"
  "sddm|Display manager|"
  "cloud-init|Cloud provisioning on every boot - not needed on bare metal|vm"
  "lxd-agent|LXD VM agent - not needed on bare metal|vm"
  "open-vm-tools|VMware guest tools outside VMware|vmware"
  "ubuntu-advantage|Ubuntu Pro status service|"
  "motd-news.timer|Fetches news/ads for the login banner|"
  "ua-timer.timer|Ubuntu Pro status polling|"
)

found=0
for entry in "${BLOAT[@]}"; do
  IFS='|' read -r unit reason scope <<<"$entry"
  [[ $unit == *.* ]] || unit="$unit.service"
  is_kept "$unit" && continue
  case $scope in
    vm)     [[ $VIRT != none ]] && continue ;;
    vmware) [[ $VIRT == vmware ]] && continue ;;
    wifi)   (( HAS_WIFI )) && continue ;;
  esac
  systemctl list-unit-files --no-legend "$unit" 2>/dev/null | grep -q . || continue

  sock=${unit%.service}.socket
  if systemctl is-active -q "$unit" 2>/dev/null ||
     { [[ $unit == *.service ]] && systemctl is-active -q "$sock" 2>/dev/null; }; then
    warn "${unit%.service} is running - $reason"
    found=1
  elif [[ $(systemctl is-enabled "$unit" 2>/dev/null) == enabled ]]; then
    info "${unit%.service} is enabled but idle - $reason"
    found=1
  fi
done
if (( found )); then
  hint "disable: sudo systemctl disable --now <unit>   (or sudo apt purge <package>)"
  hint "keep one on purpose: re-run with KEEP=\"<name> ...\""
else
  pass "None of the usual bloat services are running"
fi

if [[ $(systemctl get-default) == graphical.target ]]; then
  warn "System boots into a graphical desktop"
  hint "sudo systemctl set-default multi-user.target"
else
  pass "Boots to console (multi-user.target)"
fi

if dpkg-query -W -f='${Status}' ubuntu-desktop ubuntu-desktop-minimal 2>/dev/null | grep -q 'install ok installed'; then
  warn "A full desktop meta-package (ubuntu-desktop) is installed"
fi

if command -v snap >/dev/null 2>&1 && systemctl is-active -q snapd 2>/dev/null; then
  mapfile -t snaps < <(snap list 2>/dev/null |
    awk 'NR>1 && $1 !~ /^(core[0-9]*|snapd|bare|gtk-common-themes|gnome-[0-9-]+|mesa-.*)$/ {print $1}')
  if ((${#snaps[@]})); then
    info "Snaps in use: ${snaps[*]}"
  else
    info "snapd is running but only base snaps are installed - likely removable"
  fi
fi

mapfile -t failed < <(systemctl --failed --no-legend --plain 2>/dev/null | awk '{print $1}')
if ((${#failed[@]})); then
  warn "Failed units: ${failed[*]}"
  hint "inspect: systemctl status <unit>"
else
  pass "No failed systemd units"
fi

nsvc=$(systemctl list-units --type=service --state=running --no-legend 2>/dev/null | wc -l)
info "$nsvc services running; memory $(free -m | awk '/^Mem:/{printf "%d/%d MiB used", $3, $2}')"
info "Top memory users:"
ps -eo rss=,comm= --sort=-rss | head -n 8 | awk '{printf "           %7.1f MiB  %s\n", $1/1024, $2}'

# ---------------------------------------------------------------------------
section "4. Our components"

sub() { printf '  %s%s%s\n' "$B" "$*" "$N"; }

is_loopback() {
  case $1 in 127.*|'[::1]'*|localhost*) return 0 ;; esac
  return 1
}

# value of KEY in a KEY=value env file
env_get() { sed -n "s/^$2=//p" "$1" 2>/dev/null | head -n1; }

# quoted value of key in a TOML [section] ("" = top level)
toml_get() {
  awk -F'"' -v s="$2" -v k="$3" 'BEGIN{in_s=(s=="")} /^\[/{in_s=($0=="["s"]")}
    in_s && $1 ~ "^[[:space:]]*"k"[[:space:]]*=" {print $2; exit}' "$1" 2>/dev/null
}

# address after -listen in a systemd unit we wrote
unit_listen() { grep -oE -- '-listen [^ ]+' "/etc/systemd/system/$1.service" 2>/dev/null | awk '{print $2; exit}'; }

# returns 1 when the unit isn't running, so callers can skip health checks
unit_ok() {
  if systemctl is-active -q "$1" 2>/dev/null; then
    pass "$1 is running"
  else
    fail "$1 is not running"
    hint "systemctl status $1; journalctl -u $1 -n 50"
    return 1
  fi
  if [[ $(systemctl is-enabled "$1" 2>/dev/null) != enabled ]]; then
    warn "$1 is not enabled at boot"
    hint "sudo systemctl enable $1"
  fi
  return 0
}

# files holding secrets must keep the mode/owner the install scripts set
perms() {
  local f=$1 mode=$2 owner=$3 got
  if [[ ! -e $f ]]; then fail "$f is missing"; return; fi
  got=$(stat -c '%a %U:%G' "$f")
  if [[ $got == "$mode $owner" ]]; then
    pass "$f is $got"
  else
    fail "$f is $got, expected $mode $owner"
    hint "sudo chown $owner $f && sudo chmod $mode $f"
  fi
}

http_ok() {
  local name=$1 url=$2; shift 2
  command -v curl >/dev/null 2>&1 || { info "curl not installed - skipping $name health check"; return; }
  if curl -fsS -m 5 -o /dev/null "$@" "$url" 2>/dev/null; then
    pass "$name answers on $url"
  else
    fail "$name does not answer on $url"
  fi
}

ours=0

if [[ -x /usr/local/bin/garage ]]; then
  ours=1; sub "garage"
  if unit_ok garage; then
    if ! st=$(/usr/local/bin/garage status 2>&1); then
      fail "garage status failed: $(tail -n1 <<<"$st")"
    elif grep -q "NO ROLE ASSIGNED" <<<"$st"; then
      warn "garage node has no layout role - it stores nothing"
      hint "re-run garage-setup.sh"
    else
      pass "garage node has a layout role"
    fi
  fi
  perms /etc/garage.toml 640 root:garage
  rpc=$(toml_get /etc/garage.toml "" rpc_bind_addr)
  admin=$(toml_get /etc/garage.toml admin api_bind_addr)
  for pair in "RPC:$rpc" "admin API:$admin"; do
    what=${pair%%:*}; addr=${pair#*:}
    [[ -z $addr ]] && continue
    if is_loopback "$addr"; then
      pass "garage $what bound to loopback ($addr)"
    else
      warn "garage $what bound to $addr - only needed for multi-node or remote admin"
      hint "set it to 127.0.0.1:<port> in /etc/garage.toml"
    fi
  done
fi

if [[ -x /usr/local/bin/garage-ui ]]; then
  ours=1; sub "garage-ui"
  listen=$(unit_listen garage-ui); port=${listen##*:}
  auth=$(env_get /etc/garage-ui.env GARAGE_UI_AUTH)
  if unit_ok garage-ui && [[ -n $port ]]; then
    http_ok garage-ui "http://127.0.0.1:$port/api/buckets" -u "$auth"
  fi
  perms /etc/garage-ui.env 600 root:root
  [[ -z $auth ]] && fail "garage-ui has no login set (GARAGE_UI_AUTH in /etc/garage-ui.env)"
fi

if [[ -x /usr/local/bin/llama-server ]]; then
  ours=1; sub "llama-server"
  ENVF=/etc/llama-server.env
  host=$(env_get $ENVF LLAMA_ARG_HOST); port=$(env_get $ENVF LLAMA_ARG_PORT)
  model=$(env_get $ENVF LLAMA_ARG_MODEL); key=$(env_get $ENVF LLAMA_API_KEY)
  if unit_ok llama-server && [[ -n $port ]]; then
    case ${host:-127.0.0.1} in 0.0.0.0|::|'[::]') h=127.0.0.1 ;; *) h=${host:-127.0.0.1} ;; esac
    http_ok llama-server "http://$h:$port/health"
  fi
  perms $ENVF 600 root:root
  if [[ -n $model && ! -s $model ]]; then
    fail "llama model $model is missing"
  fi
  if [[ -n $host ]] && ! is_loopback "$host"; then
    if [[ -n $key ]]; then
      pass "llama-server listens on $host with an API key"
    else
      fail "llama-server listens on $host without an API key"
      hint "re-run llama-setup.sh with API_KEY=... (or HOST=127.0.0.1)"
    fi
  fi
fi

if [[ -x /usr/local/bin/duckpipe ]]; then
  ours=1; sub "duckpipe"
  unit_ok duckpipe
  perms /etc/duckpipe.env 600 root:root
  perms /etc/duckpipe.yaml 640 root:duckpipe
  [[ -x /usr/local/bin/duckdb ]] || fail "duckdb CLI is missing (/usr/local/bin/duckdb) - duckpipe needs it"
fi

if [[ -x /usr/local/bin/ducklake ]]; then
  ours=1; sub "ducklake"
  pgver=$(pg_lsclusters -h 2>/dev/null | awk '$2 == "main" {print $1}' | sort -V | tail -n1)
  if [[ -z $pgver ]]; then
    fail "no PostgreSQL 'main' cluster - the lake's catalog is gone"
    hint "re-run ducklake-setup.sh"
  elif unit_ok "postgresql@$pgver-main"; then
    if out=$(timeout 60 /usr/local/bin/ducklake -batch -csv -noheader \
        -c "SELECT value FROM ducklake_options('lake') WHERE option_name = 'data_path'" </dev/null 2>&1) &&
       [[ $(tail -n1 <<<"$out") == s3://* ]]; then
      pass "lake opens (data in $(tail -n1 <<<"$out"))"
    else
      fail "lake does not open: $(tail -n1 <<<"$out")"
      hint "sudo ducklake -c 'SHOW ALL TABLES'"
    fi
  fi
  perms /etc/ducklake.env 600 root:root
  # the catalog listens on the LAN, so every network rule must be LAN-only and need a password
  hba=/etc/postgresql/$pgver/main/pg_hba.conf
  if [[ -n $pgver && -r $hba ]]; then
    bad=$(awk '$1 ~ /^host/ && ($5 == "trust" || $6 == "trust" || $4 == "all") {print}' "$hba")
    while read -r _ _ _ src _; do
      [[ $src == */* ]] && ! is_private "$src" && ! is_trusted "$src" && bad+=$'\n'"source $src"
    done < <(grep -E '^host' "$hba")
    if [[ -n ${bad//$'\n'/} ]]; then
      fail "pg_hba.conf has network rules that are open or need no password"
      while read -r l; do [[ -n $l ]] && hint "$l"; done <<<"$bad"
    else
      pass "pg_hba.conf network rules are LAN/loopback only and need a password"
    fi
  fi
  if grep -qE '^[[:space:]]*catalog:[[:space:]]*ducklake:sqlite:' /etc/duckpipe.yaml 2>/dev/null; then
    info "duckpipe still uses its own SQLite catalog, not this lake (/etc/duckpipe.yaml)"
  fi
fi

if [[ -x /usr/local/bin/vigil ]]; then
  ours=1; sub "vigil"
  listen=$(unit_listen vigil); port=${listen##*:}
  if unit_ok vigil && [[ -n $port ]]; then
    http_ok vigil "http://127.0.0.1:$port/healthz"
  fi
  perms /etc/vigil.env 600 root:root
  perms /etc/vigil-client.env 644 root:root
fi

if [[ -x /usr/local/bin/relay ]]; then
  ours=1; sub "relay"
  listen=$(unit_listen relay); port=${listen##*:}
  token=$(env_get /etc/relay.env RELAY_TOKEN)
  if unit_ok relay && [[ -n $port ]]; then
    http_ok relay "http://127.0.0.1:$port/v1/topics" -H "Authorization: Bearer $token"
  fi
  perms /etc/relay.env 600 root:root
  perms /etc/relay-client.env 644 root:root
  if [[ -z $token ]]; then
    if [[ -n $listen ]] && ! is_loopback "$listen"; then
      fail "relay listens on $listen without a token - anyone on the network can publish and read"
    else
      warn "relay has no token (RELAY_TOKEN in /etc/relay.env)"
    fi
    hint "delete /etc/relay.env and /etc/relay-client.env, then re-run relay/deploy.sh to generate one"
  fi
fi

mapfile -t u2b < <(compgen -G '/home/*/.local/bin/url-to-bucket'; compgen -G '/root/.local/bin/url-to-bucket')
if ((${#u2b[@]})); then
  ours=1; sub "url-to-bucket"
  info "installed at ${u2b[*]}"
fi

(( ours )) || info "None of our components (garage, garage-ui, llama-server, duckpipe, ducklake, vigil, relay, url-to-bucket) are installed"

# ---------------------------------------------------------------------------
section "Summary"
printf '  %s%d pass%s   %s%d warn%s   %s%d fail%s\n' "$G" "$PASS" "$N" "$Y" "$WARN" "$N" "$R" "$FAIL" "$N"
(( FAIL )) && exit 2
(( WARN )) && exit 1
exit 0
