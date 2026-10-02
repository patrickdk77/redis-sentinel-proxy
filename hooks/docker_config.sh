docker_config_needed() {
  local cfg="$HOME/.docker/config.json"
  [ -n "${DOCKERCFG:-}" ] || return 1
  [ -e "$cfg" ] || return 0
  ! grep -q 'experimental.*enabled' "$cfg"
}

update_docker_config() {
  local cfg="$HOME/.docker/config.json"
  local current='{}'
  local tmp
  mkdir -p "$HOME/.docker"
  if [ -e "$cfg" ]; then
    current=$(cat "$cfg")
  fi
  tmp=$(mktemp "$cfg.XXXXXX")
  if printf '%s' "$current" | jq --argjson auths "$DOCKERCFG" \
    '.auths = ((.auths // {}) + $auths)
     | .experimental = "enabled"' > "$tmp"; then
    mv "$tmp" "$cfg"
  else
    rm -f "$tmp"
    return 1
  fi
}
