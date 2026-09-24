package main

import (
	"sort"
	"strconv"
	"strings"
)

// Stremio has no structured quality field, so every signal here is a
// best-effort heuristic read from the addon's own free-text title/filename.
// A stream with no parseable signal scores 0 throughout and simply keeps its
// relative addon-order position rather than being penalized or dropped.
var (
	resolution4320Pattern = mustCompileWordPattern("8k", "4320p")
	resolution2160Pattern = mustCompileWordPattern("4k", "2160p", "uhd")
	resolution1080Pattern = mustCompileWordPattern("1080p", "1080i")
	resolution720Pattern  = mustCompileWordPattern("720p")
	resolution480Pattern  = mustCompileWordPattern("480p", "360p", "sd")

	hdrPattern       = mustCompileWordPattern("hdr10plus", "hdr10", "hdr", "dolbyvision", "dv")
	codecAV1Pattern  = mustCompileWordPattern("av1")
	codecHEVCPattern = mustCompileWordPattern("hevc", "x265", "h265")
	codecAVCPattern  = mustCompileWordPattern("x264", "h264", "avc")
)

// streamQuality is the ranking signal extracted from a stream's own
// self-reported text.
type streamQuality struct {
	resolution int // higher is better; 0 = unknown
	hdr        bool
	codec      int   // higher is more modern/efficient; 0 = unknown
	size       int64 // bytes, same-resolution tiebreaker
}

// normalizeForMatching strips everything but letters/digits and lowercases,
// so "H.264", "h 264" and "h264" all match the same pattern without needing
// a regexp per separator style.
func normalizeForMatching(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

type wordPattern struct {
	needles []string
}

func mustCompileWordPattern(needles ...string) wordPattern {
	return wordPattern{needles: needles}
}

func (p wordPattern) matches(normalized string) bool {
	for _, needle := range p.needles {
		if strings.Contains(normalized, needle) {
			return true
		}
	}
	return false
}

func parseStreamQuality(stream HubStream) streamQuality {
	normalized := normalizeForMatching(strings.Join(
		[]string{stream.Name, stream.Title, stream.Description, stream.Filename}, " ",
	))

	quality := streamQuality{size: stream.VideoSize}

	switch {
	case resolution4320Pattern.matches(normalized):
		quality.resolution = 5
	case resolution2160Pattern.matches(normalized):
		quality.resolution = 4
	case resolution1080Pattern.matches(normalized):
		quality.resolution = 3
	case resolution720Pattern.matches(normalized):
		quality.resolution = 2
	case resolution480Pattern.matches(normalized):
		quality.resolution = 1
	}

	quality.hdr = hdrPattern.matches(normalized)

	switch {
	case codecAV1Pattern.matches(normalized):
		quality.codec = 3
	case codecHEVCPattern.matches(normalized):
		quality.codec = 2
	case codecAVCPattern.matches(normalized):
		quality.codec = 1
	}

	return quality
}

// rankStreams sorts streams best-quality-first: resolution, then HDR, then
// codec efficiency, then file size as a same-resolution tiebreaker, and
// finally the admin's configured addon order as the fully deterministic
// last tiebreaker (matches the previous addon-order-only behavior when no
// quality signal is present at all).
func rankStreams(values []HubStream, addonOrder map[string]int) []HubStream {
	type scored struct {
		stream  HubStream
		quality streamQuality
	}

	scoredValues := make([]scored, len(values))
	for i, value := range values {
		scoredValues[i] = scored{stream: value, quality: parseStreamQuality(value)}
	}

	sort.SliceStable(scoredValues, func(i, j int) bool {
		a, b := scoredValues[i].quality, scoredValues[j].quality
		if a.resolution != b.resolution {
			return a.resolution > b.resolution
		}
		if a.hdr != b.hdr {
			return a.hdr
		}
		if a.codec != b.codec {
			return a.codec > b.codec
		}
		if a.size != b.size {
			return a.size > b.size
		}
		return addonOrder[scoredValues[i].stream.AddonID] < addonOrder[scoredValues[j].stream.AddonID]
	})

	result := make([]HubStream, len(scoredValues))
	for i, value := range scoredValues {
		result[i] = value.stream
	}
	return result
}

// dedupeStreamMirrors collapses same-addon streams that are almost
// certainly the same release pointed at a different mirror URL (identical
// filename and size), keeping the first. Different addons are always kept
// as independent options, even with matching metadata, since they are
// genuinely different sources a user may want as a fallback.
func dedupeStreamMirrors(values []HubStream) []HubStream {
	seen := map[string]bool{}
	result := make([]HubStream, 0, len(values))
	for _, value := range values {
		signature := value.AddonID
		if value.Filename != "" {
			signature += "\x00" + value.Filename + "\x00" + strconv.FormatInt(value.VideoSize, 10)
		} else {
			signature += "\x00" + value.URL
		}
		if seen[signature] {
			continue
		}
		seen[signature] = true
		result = append(result, value)
	}
	return result
}
