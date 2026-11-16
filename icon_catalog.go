package main

import "strings"

// Bundled artwork provenance is recorded in third_party/dashboard-icons/manifest.json.
type serviceIcon struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Category string `json:"category"`
}

var bundledServices = []serviceIcon{
	{"radarr", "Radarr", "Media automation"},
	{"sonarr", "Sonarr", "Media automation"},
	{"lidarr", "Lidarr", "Media automation"},
	{"readarr", "Readarr", "Media automation"},
	{"prowlarr", "Prowlarr", "Media automation"},
	{"bazarr", "Bazarr", "Media automation"},
	{"whisparr", "Whisparr", "Media automation"},
	{"recyclarr", "Recyclarr", "Media automation"},
	{"unpackerr", "Unpackerr", "Media automation"},
	{"huntarr", "Huntarr", "Media automation"},
	{"maintainerr", "Maintainerr", "Media automation"},
	{"seerr", "Seerr", "Media automation"},
	{"overseerr", "Overseerr", "Media automation"},
	{"jellyseerr", "Jellyseerr", "Media automation"},
	{"plex", "Plex", "Media libraries"},
	{"jellyfin", "Jellyfin", "Media libraries"},
	{"emby", "Emby", "Media libraries"},
	{"tautulli", "Tautulli", "Media libraries"},
	{"audiobookshelf", "Audiobookshelf", "Media libraries"},
	{"navidrome", "Navidrome", "Media libraries"},
	{"calibre-web", "Calibre-Web", "Media libraries"},
	{"kavita", "Kavita", "Media libraries"},
	{"komga", "Komga", "Media libraries"},
	{"immich", "Immich", "Media libraries"},
	{"qbittorrent", "qBittorrent", "Downloads"},
	{"transmission", "Transmission", "Downloads"},
	{"deluge", "Deluge", "Downloads"},
	{"sabnzbd", "SABnzbd", "Downloads"},
	{"nzbget", "NZBGet", "Downloads"},
	{"home-assistant", "Home Assistant", "Home automation"},
	{"homebridge", "Homebridge", "Home automation"},
	{"zigbee2mqtt", "Zigbee2MQTT", "Home automation"},
	{"mosquitto", "Mosquitto", "Home automation"},
	{"frigate", "Frigate", "Home automation"},
	{"node-red", "Node-RED", "Home automation"},
	{"ntfy", "ntfy", "Monitoring"},
	{"uptime-kuma", "Uptime Kuma", "Monitoring"},
	{"beszel", "Beszel", "Monitoring"},
	{"grafana", "Grafana", "Monitoring"},
	{"prometheus", "Prometheus", "Monitoring"},
	{"dozzle", "Dozzle", "Monitoring"},
	{"speedtest-tracker", "Speedtest Tracker", "Monitoring"},
	{"portainer", "Portainer", "Infrastructure"},
	{"dockge", "Dockge", "Infrastructure"},
	{"docker", "Docker", "Infrastructure"},
	{"unraid", "Unraid", "Infrastructure"},
	{"truenas", "TrueNAS", "Infrastructure"},
	{"proxmox", "Proxmox", "Infrastructure"},
	{"tailscale", "Tailscale", "Infrastructure"},
	{"wireguard", "WireGuard", "Infrastructure"},
	{"adguard-home", "AdGuard Home", "Infrastructure"},
	{"pi-hole", "Pi-hole", "Infrastructure"},
	{"nginx-proxy-manager", "Nginx Proxy Manager", "Infrastructure"},
	{"traefik", "Traefik", "Infrastructure"},
	{"caddy", "Caddy", "Infrastructure"},
	{"authentik", "Authentik", "Infrastructure"},
	{"authelia", "Authelia", "Infrastructure"},
	{"nextcloud", "Nextcloud", "Files and productivity"},
	{"syncthing", "Syncthing", "Files and productivity"},
	{"vaultwarden", "Vaultwarden", "Files and productivity"},
	{"paperless-ngx", "Paperless-ngx", "Files and productivity"},
	{"filebrowser", "File Browser", "Files and productivity"},
	{"mealie", "Mealie", "Files and productivity"},
	{"gitea", "Gitea", "Files and productivity"},
	{"forgejo", "Forgejo", "Files and productivity"},
	{"it-tools", "IT-Tools", "Files and productivity"},
	{"freshrss", "FreshRSS", "Files and productivity"},
	{"linkwarden", "Linkwarden", "Files and productivity"},
	{"actual-budget", "Actual Budget", "Files and productivity"},
	{"vikunja", "Vikunja", "Files and productivity"},
	{"n8n", "n8n", "Files and productivity"},
	{"homepage", "Homepage", "Files and productivity"},
	{"homarr", "Homarr", "Files and productivity"},
}

func normalizedServiceName(name string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
		}
	}
	return out.String()
}
func defaultServiceIcon(name string) string {
	key := normalizedServiceName(name)
	if key == "" {
		return ""
	}
	for _, icon := range bundledServices {
		if key == normalizedServiceName(icon.Name) || key == normalizedServiceName(icon.Label) {
			return icon.Name
		}
	}
	return ""
}
