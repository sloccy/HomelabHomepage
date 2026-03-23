package web

import (
	"regexp"
	"strings"
)

var (
	reCSSComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	reSpaces     = regexp.MustCompile(`\s+`)
)

// minifyCSS strips block comments and collapses whitespace/punctuation
// spacing. Safe for the subset of CSS used in this project; does not
// handle strings whose values contain { } ; , characters.
func minifyCSS(src string) string {
	s := reCSSComment.ReplaceAllString(src, "")
	s = reSpaces.ReplaceAllString(s, " ")
	// Remove spaces around punctuation (safe after whitespace collapse).
	s = strings.ReplaceAll(s, " {", "{")
	s = strings.ReplaceAll(s, "{ ", "{")
	s = strings.ReplaceAll(s, " }", "}")
	s = strings.ReplaceAll(s, "} ", "}")
	s = strings.ReplaceAll(s, " ;", ";")
	s = strings.ReplaceAll(s, "; ", ";")
	s = strings.ReplaceAll(s, " ,", ",")
	s = strings.ReplaceAll(s, ", ", ",")
	// Remove trailing semicolons before closing brace.
	s = strings.ReplaceAll(s, ";}", "}")
	return strings.TrimSpace(s)
}

// minifyJS strips block comments and collapses whitespace.
func minifyJS(src string) string {
	s := reCSSComment.ReplaceAllString(src, "")
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
