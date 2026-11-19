package main

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var durationPattern = regexp.MustCompile(`^([0-9]{1,9})([smhd]?)$`)

// ParseCooldown reads 90, 90s, 30m, 12h or 3d.
func ParseCooldown(text string) (time.Duration, error) {
	if text == "" {
		return 0, nil
	}
	match := durationPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, fmt.Errorf("COOLDOWN must be a time such as 3d, 12h, 30m or 90 (seconds)")
	}
	n, _ := strconv.Atoi(match[1])
	unit := map[string]time.Duration{"": time.Second, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[match[2]]
	return time.Duration(n) * unit, nil
}

// Window is a time of day range, such as 02:00-05:00; 22:00-02:00 spans
// midnight. Opens and Closes are minutes since midnight.
type Window struct {
	Opens, Closes int
	Text          string
}

var windowPattern = regexp.MustCompile(`^([0-9]{2}):([0-5][0-9])-([0-9]{2}):([0-5][0-9])$`)

// ParseWindow reads HH:MM-HH:MM; an empty text means no window.
func ParseWindow(text string) (*Window, error) {
	if text == "" {
		return nil, nil
	}
	match := windowPattern.FindStringSubmatch(text)
	bad := fmt.Errorf("WINDOW must be two different times of day such as 02:00-05:00")
	if match == nil {
		return nil, bad
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	if n(match[1]) > 23 || n(match[3]) > 23 {
		return nil, bad
	}
	w := &Window{Opens: n(match[1])*60 + n(match[2]), Closes: n(match[3])*60 + n(match[4]), Text: text}
	if w.Opens == w.Closes {
		return nil, bad
	}
	return w, nil
}

func minuteOfDay(t time.Time) int { return t.Hour()*60 + t.Minute() }

// Contains tells whether the window is open at t (in t's location).
func (w *Window) Contains(t time.Time) bool {
	if w == nil {
		return true
	}
	m := minuteOfDay(t)
	if w.Opens < w.Closes {
		return m >= w.Opens && m < w.Closes
	}
	return m >= w.Opens || m < w.Closes
}

// NextOpen is t when the window is open at t, otherwise the next time it opens.
func (w *Window) NextOpen(t time.Time) time.Time {
	if w.Contains(t) {
		return t
	}
	opens := time.Date(t.Year(), t.Month(), t.Day(), w.Opens/60, w.Opens%60, 0, 0, t.Location())
	if !opens.After(t) {
		opens = opens.AddDate(0, 0, 1)
	}
	return opens
}
