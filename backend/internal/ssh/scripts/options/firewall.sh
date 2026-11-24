# OPTIONAL — UFW firewall. Appended to the bootstrap only if the operator opts in.
log "option: configuring ufw firewall (deny inbound except ssh/http/https)"
apt-get install -y ufw
ufw default deny incoming
ufw default allow outgoing
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
# Keep anything the server already serves publicly reachable, so turning the
# firewall on never cuts off a running service. Review and close these later
# with "ufw delete allow <port>/tcp" if they shouldn't be public.
for port in $(ss -Hltn 2>/dev/null | awk '{print $4}' | grep -vE '^(127\.|\[::1\]|::1)' | sed -E 's/.*:([0-9]+)$/\1/' | sort -un); do
  case "$port" in 22|80|443) continue ;; esac
  log "keeping port $port open (already in use on this server)"
  ufw allow "$port/tcp"
done
ufw --force enable
