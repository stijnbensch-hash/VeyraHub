package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type stremioMeta struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Poster      string          `json:"poster"`
	Background  string          `json:"background"`
	ReleaseInfo string          `json:"releaseInfo"`
	Year        json.RawMessage `json:"year"`
	Genres      []string        `json:"genres"`
	Videos      []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Name      string `json:"name"`
		Season    int    `json:"season"`
		Episode   int    `json:"episode"`
		Thumbnail string `json:"thumbnail"`
	} `json:"videos"`
}

func (h *Hub) fetchCatalog(ctx context.Context, addon Addon, catalog AddonCatalog, search string, skip int) ([]stremioMeta, error) {
	cacheKey := addon.ID + "\x00" + catalog.Type + "\x00" + catalog.ID + "\x00" + search + "\x00" + strconv.Itoa(skip)
	if cached, ok := h.catalogCache.get(cacheKey); ok {
		return cached, nil
	}

	base, err := url.Parse(addon.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = path.Join(base.Path, "catalog", catalog.Type, catalog.ID)
	var extras []string
	if skip > 0 {
		extras = append(extras, "skip="+strconv.Itoa(skip))
	}
	if search != "" && contains(catalog.Extra, "search") {
		extras = append(extras, "search="+url.QueryEscape(search))
	}
	if len(extras) > 0 {
		base.Path += "/" + strings.Join(extras, "&") + ".json"
	} else {
		base.Path += ".json"
	}
	// Per-addon deadline (spec: one slow/broken addon must not block the
	// whole request) -- bounds this one HTTP call regardless of how long
	// the caller's own ctx still has left.
	addonCtx, cancel := context.WithTimeout(ctx, h.addonTimeout)
	defer cancel()
	start := time.Now()
	request, _ := http.NewRequestWithContext(addonCtx, http.MethodGet, base.String(), nil)
	response, err := h.client.Do(request)
	if err != nil {
		h.recordAddonHealthTimed(addon.ID, false, sanitizeAddonError(err), time.Since(start))
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		err := fmt.Errorf("catalog HTTP %d", response.StatusCode)
		h.recordAddonHealthTimed(addon.ID, false, sanitizeAddonError(err), time.Since(start))
		return nil, err
	}
	var payload struct {
		Metas []stremioMeta `json:"metas"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload)
	if err != nil {
		h.recordAddonHealthTimed(addon.ID, false, sanitizeAddonError(err), time.Since(start))
		return nil, err
	}
	h.recordAddonHealthTimed(addon.ID, true, "", time.Since(start))
	h.catalogCache.set(cacheKey, payload.Metas)
	return payload.Metas, nil
}

func (h *Hub) fetchMeta(ctx context.Context, preferredAddonID, mediaType, id string) (stremioMeta, error) {
	meta, _, err := h.fetchMetaWithSource(ctx, preferredAddonID, mediaType, id)
	return meta, err
}

func (h *Hub) fetchMetaWithSource(ctx context.Context, preferredAddonID, mediaType, id string) (stremioMeta, string, error) {
	addons := h.addonCatalog()

	for pass := 0; pass < 2; pass++ {
		for _, addon := range addons {
			if !addon.Enabled || !contains(addon.Resources, "meta") || (pass == 0) != (addon.ID == preferredAddonID) {
				continue
			}

			base, err := url.Parse(addon.BaseURL)
			if err != nil {
				continue
			}

			base.Path = path.Join(base.Path, "meta", mediaType, id+".json")

			// Per-addon deadline -- bounds this one call; cancel() runs when
			// fetchMetaWithSource itself returns (either on this addon's
			// success or after the whole two-pass loop below gives up).
			addonCtx, cancel := context.WithTimeout(ctx, h.addonTimeout)
			defer cancel()
			start := time.Now()

			request, err := http.NewRequestWithContext(addonCtx, http.MethodGet, base.String(), nil)
			if err != nil {
				continue
			}

			response, err := h.client.Do(request)
			if err != nil {
				h.recordAddonHealthTimed(addon.ID, false, sanitizeAddonError(err), time.Since(start))
				continue
			}

			if response.StatusCode < 200 || response.StatusCode >= 300 {
				response.Body.Close()
				h.recordAddonHealthTimed(
					addon.ID,
					false,
					fmt.Sprintf("metadata request returned HTTP %d", response.StatusCode),
					time.Since(start),
				)
				continue
			}

			var payload struct {
				Meta stremioMeta `json:"meta"`
			}

			err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload)
			response.Body.Close()

			if err != nil {
				h.recordAddonHealthTimed(addon.ID, false, sanitizeAddonError(err), time.Since(start))
				continue
			}

			if payload.Meta.ID == "" {
				h.recordAddonHealthTimed(addon.ID, false, "metadata response contained no media item", time.Since(start))
				continue
			}

			h.recordAddonHealthTimed(addon.ID, true, "", time.Since(start))
			return payload.Meta, addon.ID, nil
		}
	}

	return stremioMeta{}, "", fmt.Errorf("metadata not found")
}
