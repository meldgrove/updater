package main

import (
	"regexp"
	"strconv"
)

// Version is a release X.Y.Z or a preview X.Y.Z-preview.N of a Meldgrove
// image. A preview sorts before the release of the same X.Y.Z, as in semver:
// 0.2.1-preview.3 is three commits past 0.2.0 and comes before 0.2.1.
type Version struct {
	Major, Minor, Patch int
	Preview             int // 0: a release
	Text                string
}

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(?:-preview\.([1-9][0-9]{0,8}))?$`)

// ParseVersion reads a release or preview version; anything else (latest,
// 0.2, 1.0.0-rc.1) is not one.
func ParseVersion(text string) (Version, bool) {
	match := versionPattern.FindStringSubmatch(text)
	if match == nil {
		return Version{}, false
	}
	number := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	v := Version{Major: number(match[1]), Minor: number(match[2]), Patch: number(match[3]), Text: text}
	if match[4] != "" {
		v.Preview = number(match[4])
	}
	return v, true
}

func (v Version) IsPreview() bool { return v.Preview > 0 }

// Compare is -1, 0 or 1 as v is older than, the same as, or newer than o.
func (v Version) Compare(o Version) int {
	for _, pair := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case v.Preview == o.Preview:
		return 0
	case v.Preview == 0: // a release is newer than its previews
		return 1
	case o.Preview == 0:
		return -1
	case v.Preview < o.Preview:
		return -1
	default:
		return 1
	}
}

// Series is what an image tag that moves without operator action follows:
// "0.2" below 1.0.0, "1" from 1.0.0. A release that needs upgrade steps
// starts a new series.
func (v Version) Series() string {
	if v.Major == 0 {
		return "0." + strconv.Itoa(v.Minor)
	}
	return strconv.Itoa(v.Major)
}

// Newest returns the newest version, or false for an empty list.
func Newest(versions []Version) (Version, bool) {
	var best Version
	found := false
	for _, v := range versions {
		if !found || v.Compare(best) > 0 {
			best, found = v, true
		}
	}
	return best, found
}

// reachedBy tells whether an image tag of one to three numbers (0.3, 1,
// 1.2.3) follows a release: 0.3 reaches 0.3.7 but not 0.4.0. Tags that are
// no number (latest) follow everything.
func reachedBy(tag string, v Version) bool {
	parts := regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?$`).FindStringSubmatch(tag)
	if parts == nil {
		return true
	}
	numbers := []int{v.Major, v.Minor, v.Patch}
	for i, part := range parts[1:] {
		if part == "" {
			break
		}
		n, _ := strconv.Atoi(part)
		if n != numbers[i] {
			return false
		}
	}
	return true
}
