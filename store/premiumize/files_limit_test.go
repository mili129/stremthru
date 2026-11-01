package premiumize

import (
	"slices"
	"testing"
)

func TestDetectResolution(t *testing.T) {
	for name, want := range map[string]string{
		"The.Matrix.1999.2160p.UHD.BluRay.REMUX.HDR.HEVC": "2160p",
		"The Matrix (1999) 4K HDR":                        "2160p",
		"The_Matrix_1999_UHD_BluRay":                      "2160p",
		"The.Matrix.1999.1080p.BluRay.x264":               "1080p",
		"The_Matrix_1999_1080p_BluRay":                    "1080p",
		"Matrix.1999.1080i.HDTV":                          "1080p",
		"The.Matrix.1999.720p.BRRip":                      "720p",
		"The.Matrix.1999.480p.DVDRip":                     "480p",
		"The.Matrix.1999.DVDRip.XviD":                     "unknown",
		"Matrix.1999.x1080p.weird":                        "unknown",
		"Movie.Title.1080p.and.2160p.Pack":                "1080p",
		"Avengers.Endgame.2019.IMAX.2160p":                "2160p",
		"Taken.4kids.edition":                             "unknown",
	} {
		if got := detectResolution(name); got != want {
			t.Errorf("detectResolution(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestParseFilesLimits(t *testing.T) {
	limits, err := parseFilesLimits(" 2160p:25, 1080P:25,720p:0,unknown:-1 ,")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"2160p": 25, "1080p": 25, "720p": 0, "unknown": -1}
	if len(limits) != len(want) {
		t.Fatalf("got %v, want %v", limits, want)
	}
	for k, v := range want {
		if limits[k] != v {
			t.Errorf("limits[%s] = %d, want %d", k, limits[k], v)
		}
	}

	if limits, err := parseFilesLimits(""); err != nil || len(limits) != 0 {
		t.Errorf("empty value: got %v, %v", limits, err)
	}
	for _, bad := range []string{"2160p", "8k:5", "1080p:x", "1080p:-2"} {
		if _, err := parseFilesLimits(bad); err == nil {
			t.Errorf("parseFilesLimits(%q) should fail", bad)
		}
	}
}

func TestSelectForFiles(t *testing.T) {
	candidates := []filesCandidate{
		{name: "A.2160p", size: 40}, // 0
		{name: "B.2160p", size: 60}, // 1
		{name: "C.2160p", size: 50}, // 2
		{name: "D.1080p", size: 10}, // 3
		{name: "E.1080p", size: 30}, // 4
		{name: "F.720p", size: 5},   // 5
		{name: "G.720p", size: 6},   // 6
		{name: "H.DVDRip", size: 1}, // 7
		{name: "I.480p", size: 2},   // 8
		{name: "J.2160p", size: -1}, // 9 unknown size goes last
	}
	limits := map[string]int{"2160p": 2, "1080p": 5, "720p": 0, "unknown": 1}
	// 2160p: two largest (B, C); 1080p: both (under limit); 720p: none;
	// unknown: one; 480p: not listed -> no limit.
	want := []int{1, 2, 3, 4, 7, 8}
	if got := selectForFiles(candidates, limits); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	all := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	if got := selectForFiles(candidates, map[string]int{}); !slices.Equal(got, all) {
		t.Errorf("no limits: got %v, want %v", got, all)
	}
	if got := selectForFiles(nil, limits); len(got) != 0 {
		t.Errorf("no candidates: got %v", got)
	}
}

func TestSelectForFilesWithHints(t *testing.T) {
	candidates := []filesCandidate{
		{name: ".", size: 60, hint: "2160p"},       // 0
		{name: ".", size: 50, hint: "1080p"},       // 1
		{name: ".", size: 40},                      // 2 no hint -> unknown
		{name: "X.1080p", size: 30, hint: "2160p"}, // 3 name wins
		{name: ".", size: 20, hint: "bogus"},       // 4 bad hint -> unknown
		{name: ".", size: 10, hint: "4K"},          // 5 -> 2160p
	}
	limits := map[string]int{"2160p": 2, "1080p": 1, "unknown": 0}
	// 2160p: 0, 5; 1080p: largest of 1, 3 -> 1; unknown: none.
	want := []int{0, 1, 5}
	if got := selectForFiles(candidates, limits); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
