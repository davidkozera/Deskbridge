//go:build windows

package main

// The settings that belong to a connection: frame rate, picture quality
// and the pane's tick boxes. Each open tab has its own set and each saved
// connection keeps its own. The pane shows the set of the tab in front, or
// with no tab open, that of the connection being set up.

import (
	"strconv"
	"strings"
)

type options struct {
	fps, quality                    int
	audio, sleep, clip, video, fast bool
}

// readOptions is what the pane's controls say now.
func readOptions() options {
	return options{
		fps: comboValue(hFPS, fpsChoices), quality: comboValue(hQuality, qualityChoices),
		audio: checked(hAudio), sleep: checked(hSleep), clip: checked(hClip), video: checked(hVideo), fast: checked(hFast),
	}
}

// showOptions puts a connection's settings in the pane's controls.
func showOptions(o options) {
	setCombo(hFPS, fpsChoices, o.fps)
	setCombo(hQuality, qualityChoices, o.quality)
	setChecked(hAudio, o.audio)
	setChecked(hSleep, o.sleep)
	setChecked(hClip, o.clip)
	setChecked(hVideo, o.video)
	setChecked(hFast, o.fast)
	audioOn.Store(o.audio)
	clipOn.Store(o.clip)
}

// String is the form kept in the settings file: fps, quality, then the
// tick boxes as 1 or 0.
func (o options) String() string {
	parts := []string{strconv.Itoa(o.fps), strconv.Itoa(o.quality)}
	for _, on := range []bool{o.audio, o.sleep, o.clip, o.video, o.fast} {
		if on {
			parts = append(parts, "1")
		} else {
			parts = append(parts, "0")
		}
	}
	return strings.Join(parts, ",")
}

func parseOptions(s string) (options, bool) {
	p := strings.Split(s, ",")
	if len(p) != 7 {
		return options{}, false
	}
	fps, err1 := strconv.Atoi(p[0])
	quality, err2 := strconv.Atoi(p[1])
	if err1 != nil || err2 != nil || fps < 1 || fps > 120 || quality < 1 || quality > 100 {
		return options{}, false
	}
	return options{fps: fps, quality: quality, audio: p[2] == "1", sleep: p[3] == "1", clip: p[4] == "1", video: p[5] == "1", fast: p[6] == "1"}, true
}
