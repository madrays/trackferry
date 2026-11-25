// Package names compares song titles and artist credits.
package names

import (
	_ "embed"
	"strings"
	"unicode"

	"golang.org/x/text/width"
)

//go:embed t2s.txt
var t2sRaw string

// t2s maps traditional to simplified characters.
var t2s = func() map[rune]rune {
	lines := strings.Fields(t2sRaw)
	from, to := []rune(lines[0]), []rune(lines[1])
	m := make(map[rune]rune, len(from))
	for i := range from {
		m[from[i]] = to[i]
	}
	return m
}()

// Norm folds text for comparison: half width, lower case, simplified characters, letters
// and digits only.
func Norm(s string) string {
	var b strings.Builder
	for _, r := range width.Narrow.String(s) {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			continue
		}
		r = unicode.ToLower(r)
		if m, ok := t2s[r]; ok {
			r = m
		}
		b.WriteRune(r)
	}
	return b.String()
}

var brackets = map[rune]rune{'(': ')', '（': '）', '[': ']', '【': '】', '《': '》', '「': '」'}

// SplitTitle separates a title into its plain part and whatever it carries in brackets or
// after " - " ("海阔天空 (Live)" → "海阔天空", "Live").
func SplitTitle(s string) (plain, extra string) {
	var p, e strings.Builder
	var close rune
	for _, r := range s {
		switch {
		case close != 0:
			if r == close {
				close = 0
				e.WriteByte(' ')
			} else {
				e.WriteRune(r)
			}
		case brackets[r] != 0 && strings.TrimSpace(p.String()) != "":
			close = brackets[r]
		default:
			p.WriteRune(r)
		}
	}
	plain, extra = strings.TrimSpace(p.String()), strings.TrimSpace(e.String())
	if a, b, ok := strings.Cut(plain, " - "); ok && strings.TrimSpace(a) != "" {
		plain, extra = strings.TrimSpace(a), strings.TrimSpace(b+" "+extra)
	}
	return plain, extra
}

// Artists splits an artist credit into normalised names.
func Artists(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune("/&、,，;；|+×", r) }) {
		for _, p := range splitFeat(part) {
			if n := Norm(p); n != "" {
				out = append(out, n)
			}
			// "陈奕迅 (Eason Chan)" is also known by its plain part.
			if plain, extra := SplitTitle(p); extra != "" {
				for _, x := range []string{plain, extra} {
					if n := Norm(x); n != "" {
						out = append(out, n)
					}
				}
			}
		}
	}
	return out
}

func splitFeat(s string) []string {
	l := strings.ToLower(s)
	for _, sep := range []string{" feat.", " feat ", " ft.", " ft ", " vs. ", " vs "} {
		if i := strings.Index(l, sep); i > 0 {
			return []string{s[:i], s[i+len(sep):]}
		}
	}
	return []string{s}
}

// ArtistMatch reports whether a credit includes the wanted artist.
func ArtistMatch(credit, want string) bool {
	w := Norm(want)
	if w == "" {
		return false
	}
	if Norm(credit) == w {
		return true
	}
	for _, a := range Artists(credit) {
		if a == w {
			return true
		}
	}
	return false
}

// Other versions of a song people do not mean unless they say so.
var versions = []string{"伴奏", "instrumental", "纯音乐", "karaoke", "卡拉ok", "消音", "铃声", "片段", "demo", "dj", "remix", "混音",
	"cover", "翻唱", "翻自", "原唱", "live", "现场", "演唱会", "钢琴", "piano", "加速", "慢速", "降调", "升调", "0.8x", "试听", "抖音", "童声", "合唱版", "女声", "男声", "吉他", "口琴", "8d"}

// VersionPenalty counts version words in text that the request does not mention.
func VersionPenalty(text, request string) int {
	l, req := strings.ToLower(text), strings.ToLower(request)
	n := 0
	for _, v := range versions {
		if strings.Contains(l, v) && !strings.Contains(req, v) {
			n++
		}
	}
	return n
}

// Versions lists the version words in text.
func Versions(text string) []string {
	l := strings.ToLower(text)
	var out []string
	for _, v := range versions {
		if strings.Contains(l, v) {
			out = append(out, v)
		}
	}
	return out
}

// SameArtist reports whether two credits share a singer, under any name they give.
func SameArtist(a, b string) bool {
	if Norm(a) != "" && Norm(a) == Norm(b) {
		return true
	}
	as := Artists(a)
	for _, x := range Artists(b) {
		for _, y := range as {
			if x == y {
				return true
			}
		}
	}
	return false
}

// NearTitle reports whether two titles are the same song but for a character or two. People say
// 「雨中的恋人」 for 黄凯芹's 「雨中的恋人们」, and a platform lists what it has; the extra or
// missing character is not a different song.
//
// Only a short difference counts, and only on longer titles: two characters that differ by one
// out of three ("情人" and "爱人") are different songs, while one out of five is a slip of the
// tongue or a platform's own spelling.
func NearTitle(a, b string) bool {
	x, y := Norm(a), Norm(b)
	if x == "" || y == "" || x == y {
		return x == y
	}
	ra, rb := []rune(x), []rune(y)
	long, short := ra, rb
	if len(long) < len(short) {
		long, short = short, long
	}
	if len(long)-len(short) > 1 {
		return false
	}
	// A difference of one character in a title of five and up is a slip; below that it may be
	// another song entirely.
	if len(long) < 5 {
		return false
	}
	return editsAtMostOne(long, short)
}

// editsAtMostOne reports whether a and b are at most one insertion, deletion or substitution
// apart. b is never longer than a.
func editsAtMostOne(a, b []rune) bool {
	i, j, skipped := 0, 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			i++
			j++
			continue
		}
		if skipped > 0 {
			return false
		}
		skipped = 1
		i++ // a character of the longer title that the shorter one does not have
	}
	return skipped+len(a)-i+len(b)-j <= 1
}
