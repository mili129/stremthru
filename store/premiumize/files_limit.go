package premiumize

// [local patch] limit how many cached magnets get a file listing (directdl)
// per check, per resolution. Each directdl is one Premiumize API call and a
// popular title can have hundreds of cached torrents, which triggers
// Premiumize's rate limit (429).
//
// STREMTHRU_PREMIUMIZE_FILES_LIMITS=2160p:25,1080p:25,720p:0,unknown:10
//   n  -> at most n per check, largest first
//   0  -> none (the magnet stays "cached" but without files)
//   -1 -> no limit (same for resolutions not listed)

import (
	"cmp"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const resolutionUnknown = "unknown"

var knownResolutions = []string{"2160p", "1440p", "1080p", "720p", "576p", "480p", "360p", "240p", "144p", resolutionUnknown}

var (
	resolution4kRegex  = regexp.MustCompile(`(^|[^a-z0-9])(4k|uhd)([^a-z0-9]|$)`)
	resolutionNumRegex = regexp.MustCompile(`(^|[^a-z0-9])(2160|1440|1080|720|576|480|360|240|144)[pi]([^a-z0-9]|$)`)
)

func detectResolution(name string) string {
	name = strings.ToLower(name)
	if m := resolutionNumRegex.FindStringSubmatch(name); m != nil {
		return m[2] + "p"
	}
	if resolution4kRegex.MatchString(name) {
		return "2160p"
	}
	return resolutionUnknown
}

func parseFilesLimits(value string) (map[string]int, error) {
	limits := map[string]int{}
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		res, n, ok := strings.Cut(part, ":")
		res = strings.ToLower(strings.TrimSpace(res))
		if !ok || !slices.Contains(knownResolutions, res) {
			return nil, fmt.Errorf("invalid entry %q (expected <resolution>:<n>, resolution one of %s)", part, strings.Join(knownResolutions, ", "))
		}
		limit, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil || limit < -1 {
			return nil, fmt.Errorf("invalid limit in %q (expected -1, 0 or a positive number)", part)
		}
		limits[res] = limit
	}
	return limits, nil
}

var premiumizeFilesLimits = func() map[string]int {
	limits, err := parseFilesLimits(os.Getenv("STREMTHRU_PREMIUMIZE_FILES_LIMITS"))
	if err != nil {
		log.Error("ignoring STREMTHRU_PREMIUMIZE_FILES_LIMITS", "error", err)
		return map[string]int{}
	}
	return limits
}()

type filesCandidate struct {
	name string
	size int64
	hint string // [local patch] resolution sent by the client, used when name has none
}

// candidateResolution uses the name first; Premiumize often returns "." as the
// name, so then it falls back to the client's hint.
func candidateResolution(c filesCandidate) string {
	res := detectResolution(c.name)
	if res != resolutionUnknown {
		return res
	}
	hint := strings.ToLower(strings.TrimSpace(c.hint))
	if hint == "4k" {
		hint = "2160p"
	}
	if slices.Contains(knownResolutions, hint) {
		return hint
	}
	return resolutionUnknown
}

// selectForFiles returns the indexes of candidates that should get a file
// listing: per resolution, the largest ones up to that resolution's limit.
func selectForFiles(candidates []filesCandidate, limits map[string]int) []int {
	byRes := map[string][]int{}
	for i, c := range candidates {
		res := candidateResolution(c)
		byRes[res] = append(byRes[res], i)
	}
	selected := []int{}
	for res, idxs := range byRes {
		limit, ok := limits[res]
		if !ok || limit < 0 || limit >= len(idxs) {
			selected = append(selected, idxs...)
			continue
		}
		slices.SortStableFunc(idxs, func(a, b int) int {
			return cmp.Compare(candidates[b].size, candidates[a].size)
		})
		selected = append(selected, idxs[:limit]...)
	}
	slices.Sort(selected)
	return selected
}

func firstError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
