package server

import (
	"regexp"
	"strings"
)

var (
	dumpFlags     = regexp.MustCompile(`\s*\[[^\]]*\]`)
	parenthetical = regexp.MustCompile(`\s*\([^()]*\)`)
	versionTag    = regexp.MustCompile(`^(?:rev(?:ision)?\s+[a-z0-9.]+|v[0-9][a-z0-9.]*|(?:beta|proto(?:type)?)(?:\s+[a-z0-9.]+)?)$`)
	languageTag   = regexp.MustCompile(`^(?:en|fr|de|es|it|ja|ko|zh|nl|pt|sv|no|da|fi)(?:,\s*(?:en|fr|de|es|it|ja|ko|zh|nl|pt|sv|no|da|fi))*$`)
)

func friendlyTitle(filenameTitle string) string {
	title := dumpFlags.ReplaceAllString(filenameTitle, "")
	title = parenthetical.ReplaceAllStringFunc(title, func(tag string) string {
		value := strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimSpace(tag), "()")))
		switch value {
		case "u", "j", "e", "usa", "europe", "japan", "world", "australia", "canada", "brazil", "korea", "china", "taiwan", "asia",
			"unl", "demo":
			return ""
		}
		if versionTag.MatchString(value) || languageTag.MatchString(value) {
			return ""
		}
		return tag
	})
	title = strings.Join(strings.Fields(strings.ReplaceAll(title, "_", " ")), " ")
	if title == "" {
		return filenameTitle
	}
	return title
}
