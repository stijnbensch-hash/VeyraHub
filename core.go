package main

import (
	"context"
	"errors"
	"strings"
)

// MediaCatalog is the neutral Hub representation of an addon catalog.
// It contains no Jellyfin- or Veyra-specific fields.
type MediaCatalog struct {
	AddonID   string   `json:"addonID"`
	AddonName string   `json:"addonName"`
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Name      string   `json:"name"`
	Extra     []string `json:"extra,omitempty"`
}

// MediaVideo represents an episode/video belonging to a media item.
type MediaVideo struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Season    int    `json:"season,omitempty"`
	Episode   int    `json:"episode,omitempty"`
	Thumbnail string `json:"thumbnail,omitempty"`
}

// MediaItem is the neutral internal representation shared by native and
// compatibility APIs.
type MediaItem struct {
	AddonID     string       `json:"addonID,omitempty"`
	ID          string       `json:"id"`
	Type        string       `json:"type"`
	Title       string       `json:"title"`
	Description string       `json:"description,omitempty"`
	Poster      string       `json:"poster,omitempty"`
	Backdrop    string       `json:"backdrop,omitempty"`
	Year        int          `json:"year,omitempty"`
	Videos      []MediaVideo `json:"videos,omitempty"`
}

func mediaItemFromMeta(addonID string, meta stremioMeta) MediaItem {
	item := MediaItem{
		AddonID:     addonID,
		ID:          meta.ID,
		Type:        meta.Type,
		Title:       meta.Name,
		Description: meta.Description,
		Poster:      meta.Poster,
		Backdrop:    meta.Background,
		Year:        metaYear(meta),
	}

	if meta.Type == "series" {
		for _, video := range meta.Videos {
			if video.ID == "" {
				continue
			}

			title := strings.TrimSpace(video.Title)
			if title == "" {
				title = strings.TrimSpace(video.Name)
			}

			item.Videos = append(item.Videos, MediaVideo{
				ID:        video.ID,
				Title:     title,
				Season:    video.Season,
				Episode:   video.Episode,
				Thumbnail: video.Thumbnail,
			})
		}
	}

	return item
}

func mediaItemsFromMetas(addonID string, metas []stremioMeta) []MediaItem {
	result := make([]MediaItem, 0, len(metas))

	for _, meta := range metas {
		if meta.ID == "" || (meta.Type != "movie" && meta.Type != "series") {
			continue
		}
		result = append(result, mediaItemFromMeta(addonID, meta))
	}

	return result
}

func (h *Hub) mediaCatalogs() []MediaCatalog {
	state := h.store.Snapshot()
	var result []MediaCatalog

	for _, addon := range state.Addons {
		if !addon.Enabled || !contains(addon.Resources, "catalog") {
			continue
		}

		for _, catalog := range addon.Catalogs {
			result = append(result, MediaCatalog{
				AddonID:   addon.ID,
				AddonName: addon.Name,
				ID:        catalog.ID,
				Type:      catalog.Type,
				Name:      catalog.Name,
				Extra:     append([]string{}, catalog.Extra...),
			})
		}
	}

	if result == nil {
		result = []MediaCatalog{}
	}
	return result
}

func (h *Hub) mediaCatalogItems(
	ctx context.Context,
	addonID, mediaType, catalogID, search string,
	skip int,
) ([]MediaItem, error) {
	addon, catalog, ok := h.catalogByID(addonID, catalogID, mediaType)
	if !ok {
		return nil, errors.New("catalog not found")
	}

	metas, err := h.fetchCatalog(ctx, addon, catalog, search, skip)
	if err != nil {
		return nil, err
	}

	return mediaItemsFromMetas(addon.ID, metas), nil
}

func (h *Hub) mediaMetadata(
	ctx context.Context,
	preferredAddonID, mediaType, id string,
) (MediaItem, error) {
	if mediaType != "movie" && mediaType != "series" {
		return MediaItem{}, errors.New("invalid media type")
	}
	if strings.TrimSpace(id) == "" {
		return MediaItem{}, errors.New("missing media id")
	}

	meta, err := h.fetchMeta(ctx, preferredAddonID, mediaType, id)
	if err != nil {
		return MediaItem{}, err
	}

	return mediaItemFromMeta(preferredAddonID, meta), nil
}

func (h *Hub) mediaSearch(ctx context.Context, query string) []MediaItem {
	query = strings.TrimSpace(query)
	if query == "" {
		return []MediaItem{}
	}

	var result []MediaItem
	seen := map[string]bool{}

	for _, addon := range h.store.Snapshot().Addons {
		if !addon.Enabled || !contains(addon.Resources, "catalog") {
			continue
		}

		for _, catalog := range addon.Catalogs {
			if !contains(catalog.Extra, "search") {
				continue
			}

			metas, err := h.fetchCatalog(ctx, addon, catalog, query, 0)
			if err != nil {
				continue
			}

			for _, item := range mediaItemsFromMetas(addon.ID, metas) {
				// IDs such as IMDb IDs are often shared by multiple addons.
				// Present one neutral media item rather than duplicates from
				// every catalog provider.
				key := item.Type + "\x00" + item.ID
				if seen[key] {
					continue
				}
				seen[key] = true
				result = append(result, item)
			}
		}
	}

	if result == nil {
		result = []MediaItem{}
	}
	return result
}
