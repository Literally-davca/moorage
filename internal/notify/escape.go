package notify

import (
	"html"
	"strings"
	"unicode/utf8"
)

// Every piece of text from containers, logs and labels is escaped for the target format
// before it is put into a message, so a log line can't format, link or ping anything.

// discordEscape escapes Discord Markdown (bold, italics, code, spoilers, quotes, headings,
// lists, masked links). Mentions are disabled separately (allowed_mentions).
func discordEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, ">", `\>`,
		"[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`, "@", "@​")
	return escapeLineStarts(r.Replace(s), "-+#")
}

// escapeLineStarts escapes list and heading markers at the start of a line.
func escapeLineStarts(s, chars string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		t := strings.TrimLeft(l, " ")
		if t != "" && strings.ContainsRune(chars, rune(t[0])) {
			lines[i] = l[:len(l)-len(t)] + `\` + t
		}
	}
	return strings.Join(lines, "\n")
}

// markdownEscape escapes CommonMark (Gotify, ntfy).
func markdownEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, ">", `\>`, "<", `\<`,
		"#", `\#`, "[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`, "!", `\!`, "{", `\{`, "}", `\}`)
	// a leading "-", "+" or "=" would start a list or a heading underline
	return escapeLineStarts(r.Replace(s), "-+=")
}

// htmlEscape escapes text for HTML (Telegram, Matrix, Pushover, e-mail).
func htmlEscape(s string) string { return html.EscapeString(s) }

// slackEscape escapes the three characters Slack treats as control sequences.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// codeBlockSafe keeps a log line from closing a ``` code block.
func codeBlockSafe(s string) string { return strings.ReplaceAll(s, "```", "ˋˋˋ") }

// headerSafe removes line breaks so text can't inject headers.
func headerSafe(s string) string {
	return clip(strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r < 0x20 {
			return ' '
		}
		return r
	}, s), 200)
}

// clip shortens s to n runes, ending with "…".
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// clipLogLine keeps one log line readable in a message.
func clipLogLine(s string) string { return clip(strings.TrimRight(s, "\r"), 300) }

// discordUsername sanitises the sender name for a Discord webhook: at most 80 characters,
// without "discord" / "clyde" (Discord rejects those) and without @ # : ``` .
func discordUsername(name string) string {
	s := strings.NewReplacer("@", "", "#", "", ":", "", "```", "").Replace(name)
	for _, bad := range []string{"discord", "clyde"} {
		for {
			i := strings.Index(strings.ToLower(s), bad)
			if i < 0 {
				break
			}
			s = s[:i] + s[i+len(bad):]
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " •")
	if s == "" || strings.EqualFold(s, "everyone") || strings.EqualFold(s, "here") {
		s = "Moorage"
	}
	return clip(s, 80)
}
