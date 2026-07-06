package webui

import (
	"net/url"
	"path/filepath"
	"strings"

	"github.com/vocoder/coldarr/internal/secrets"
)

// linkView is one clickable icon in a Plan/History row's Links column.
// App selects which icon partial (icon-radarr/icon-sonarr/icon-jellyfin)
// and CSS class to render.
type linkView struct {
	App string
	URL string
}

// linkSources holds the page-render-scoped, row-independent pieces needed
// to build a Links column for any number of rows - resolved once per page
// load rather than once per row. Everything here comes from connStore and
// the linkCache (see internal/linkcache) - never a live Radarr/Sonarr/
// Jellyfin call, so building this costs nothing per page view.
type linkSources struct {
	// radarrExternal/sonarrExternal/jellyfinExternal are each app's link
	// base (External URL if set, else its regular URL), or "" if that app
	// isn't configured at all - in which case no icon for it is ever shown.
	radarrExternal   string
	sonarrExternal   string
	jellyfinExternal string

	jellyfinServerID string
	// jellyfinPathToID is nil if Jellyfin isn't configured/enabled, or if
	// the link cache hasn't been refreshed yet - either way, degrades to
	// "no Jellyfin icon" rather than an error, since this is a cosmetic
	// convenience, not a safety-relevant check like Favorites protection.
	jellyfinPathToID map[string]string
}

// linkBase returns external if set, else base, trimmed of any trailing
// slash so it's ready to have a path appended - or "" if neither is set
// (the app isn't configured).
func linkBase(base, external string) string {
	if external != "" {
		return strings.TrimRight(external, "/")
	}
	return strings.TrimRight(base, "/")
}

// buildLinkSources resolves each configured app's link base from
// connStore and, if Jellyfin is enabled, its path->itemID catalog and
// server ID from the link cache's last refresh (see internal/linkcache) -
// never live, so this is free to call on every Plan/History page view.
func (s *Server) buildLinkSources() linkSources {
	var src linkSources

	if conn, source := s.connStore.Effective("radarr"); source != secrets.SourceNone {
		src.radarrExternal = linkBase(conn.URL, conn.ExternalURL)
	}
	if conn, source := s.connStore.Effective("sonarr"); source != secrets.SourceNone {
		src.sonarrExternal = linkBase(conn.URL, conn.ExternalURL)
	}

	if conn, source := s.connStore.Effective("jellyfin"); source != secrets.SourceNone && conn.Enabled {
		src.jellyfinExternal = linkBase(conn.URL, conn.ExternalURL)
		snap := s.linkCache.Get()
		src.jellyfinPathToID = snap.JellyfinPathToID
		src.jellyfinServerID = snap.JellyfinServerID
	}

	return src
}

// itemLinks returns the Links cell for one item: the owning Arr app's
// icon (if that app is configured and titleSlug is known) plus a Jellyfin
// icon if path resolves to a known Jellyfin item. Any piece that isn't
// known is simply omitted - "show what we know," never a broken link.
func itemLinks(src linkSources, arrApp, titleSlug, path string) []linkView {
	var links []linkView

	if titleSlug != "" {
		switch arrApp {
		case "radarr":
			if src.radarrExternal != "" {
				links = append(links, linkView{App: "radarr", URL: src.radarrExternal + "/movie/" + url.PathEscape(titleSlug)})
			}
		case "sonarr":
			if src.sonarrExternal != "" {
				links = append(links, linkView{App: "sonarr", URL: src.sonarrExternal + "/series/" + url.PathEscape(titleSlug)})
			}
		}
	}

	if src.jellyfinExternal != "" && path != "" {
		if id, ok := src.jellyfinPathToID[filepath.Clean(path)]; ok {
			u := src.jellyfinExternal + "/web/index.html#/details?id=" + url.QueryEscape(id)
			if src.jellyfinServerID != "" {
				u += "&serverId=" + url.QueryEscape(src.jellyfinServerID)
			}
			links = append(links, linkView{App: "jellyfin", URL: u})
		}
	}

	return links
}
