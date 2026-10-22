package exportcheck

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

var (
	referenceDefinition = regexp.MustCompile(`(?m)^ {0,3}\[([^]\n]+)\]:[ \t]*(.*)$`)
	markdownLink        = regexp.MustCompile(`!?\[([^]\n]*)\](\(|\[([^]\n]*)\])?`)
	inlineCode          = regexp.MustCompile("(`+)([^`]*)(`+)")
)

// Mask code without moving line numbers. Links in examples are not navigation.
func markdownProse(text string) string {
	lines := strings.Split(text, "\n")
	fence := ""
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if fence != "" {
			lines[i] = ""
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, string(fence[0])+" \t") == "" {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			n := 0
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			fence = trimmed[:n]
			lines[i] = ""
		} else if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			lines[i] = ""
		} else {
			lines[i] = inlineCode.ReplaceAllStringFunc(line, func(code string) string { return strings.Repeat(" ", len(code)) })
		}
	}
	return strings.Join(lines, "\n")
}

func referenceLabel(label string) string {
	return strings.ToLower(strings.Join(strings.Fields(label), " "))
}

// Destinations can be angle-bracketed or contain escaped/balanced parentheses.
// An optional title follows whitespace and is not part of the destination.
func linkDestination(text string) string {
	text = strings.TrimLeft(text, " \t\n")
	if strings.HasPrefix(text, "<") {
		if end := strings.IndexByte(text, '>'); end >= 0 {
			return text[1:end]
		}
		return ""
	}
	var dest strings.Builder
	depth := 0
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if ch == '\\' && i+1 < len(text) {
			i++
			dest.WriteByte(text[i])
			continue
		}
		if ch == '(' {
			depth++
		} else if ch == ')' {
			if depth == 0 {
				break
			}
			depth--
		} else if ch == ' ' || ch == '\t' || ch == '\n' {
			break
		}
		dest.WriteByte(ch)
	}
	return dest.String()
}

func markdownFindings(name, text string, files snapshot) []string {
	prose := markdownProse(text)
	refs := map[string]string{}
	var findings []string
	check := func(dest string, offset int) {
		u, err := url.Parse(dest)
		if err != nil {
			findings = append(findings, fmt.Sprintf("%s: invalid Markdown destination %q", name, dest))
			return
		}
		if u.IsAbs() || u.Host != "" || u.Path == "" {
			return
		}
		target := path.Clean(path.Join(path.Dir(name), u.Path))
		if strings.HasPrefix(u.Path, "/") {
			target = strings.TrimPrefix(path.Clean(u.Path), "/")
		}
		_, exists := files[target]
		if target == "." {
			exists = true
		}
		if !exists {
			line := strings.Count(prose[:offset], "\n") + 1
			findings = append(findings, fmt.Sprintf("%s:%d: broken relative Markdown link %q", name, line, dest))
		}
	}
	for _, loc := range referenceDefinition.FindAllStringSubmatchIndex(prose, -1) {
		dest := linkDestination(prose[loc[4]:loc[5]])
		refs[referenceLabel(prose[loc[2]:loc[3]])] = dest
		check(dest, loc[0])
	}
	// Remove definitions after resolving them so their labels are not links.
	body := referenceDefinition.ReplaceAllStringFunc(prose, func(def string) string { return strings.Repeat(" ", len(def)) })
	for _, loc := range markdownLink.FindAllStringSubmatchIndex(body, -1) {
		if loc[0] > 0 && body[loc[0]-1] == '\\' {
			continue
		}
		label := referenceLabel(body[loc[2]:loc[3]])
		if loc[4] >= 0 && body[loc[4]:loc[5]] == "(" {
			check(linkDestination(body[loc[5]:]), loc[0])
		} else if loc[6] >= 0 {
			if ref := referenceLabel(body[loc[6]:loc[7]]); ref != "" {
				label = ref
			}
			if dest, ok := refs[label]; ok {
				check(dest, loc[0])
			} else {
				findings = append(findings, fmt.Sprintf("%s: undefined Markdown reference %q", name, label))
			}
		} else if dest, ok := refs[label]; ok {
			check(dest, loc[0])
		}
	}
	return findings
}
