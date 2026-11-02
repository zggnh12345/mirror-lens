package discover

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	optionTagRe  = regexp.MustCompile(`<label[^>]*class="mirror-option"[^>]*>`)
	statusAttrRe = regexp.MustCompile(`data-(url|status)="([^"]*)"`)
	groupIDRe    = regexp.MustCompile(`id="group-[a-z0-9-]+"`)
)

// parse 按格式分发，返回去重后的候选。
func parse(f feed, body []byte) []candidate {
	switch f.format {
	case fmtStatusHTML:
		return parseStatusHTML(body, f)
	case fmtStatusJSON:
		return parseStatusJSON(body, f)
	default:
		return parseMarkdown(body, f)
	}
}

func parseMarkdown(body []byte, f feed) []candidate {
	text := string(body)
	var out []candidate
	seen := map[string]bool{}
	add := func(raw string) {
		h := hostOf(raw)
		if seen[h] || !LooksLikeMirror(h, f.kind) {
			return
		}
		seen[h] = true
		out = append(out, candidate{host: h, kind: f.kind, origin: f.name})
	}
	for _, m := range urlHostRe.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range codeHostRe.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	return out
}

// parseStatusHTML 读 1Panel 这类状态页：每个分组一个 div，条目是带
// data-url/data-status 的 label。只取指定分组里 healthy 的。
func parseStatusHTML(body []byte, f feed) []candidate {
	s := string(body)
	start := strings.Index(s, `id="group-`+f.group+`"`)
	if start < 0 {
		return nil
	}
	block := s[start:]
	if next := groupIDRe.FindStringIndex(block[1:]); next != nil {
		block = block[:next[0]+1]
	}
	var out []candidate
	seen := map[string]bool{}
	for _, tag := range optionTagRe.FindAllString(block, -1) {
		var url, status string
		for _, kv := range statusAttrRe.FindAllStringSubmatch(tag, -1) {
			switch kv[1] {
			case "url":
				url = kv[2]
			case "status":
				status = kv[2]
			}
		}
		h := hostOf(url)
		if status != "healthy" || seen[h] || denied(h) {
			continue
		}
		seen[h] = true
		out = append(out, candidate{host: h, kind: f.kind, origin: f.name, online: true})
	}
	return out
}

type statusEntry struct {
	URL      string `json:"url"`
	Status   string `json:"status"`
	Official bool   `json:"official"`
	Access   string `json:"access"`
}

func parseStatusJSON(body []byte, f feed) []candidate {
	var list []statusEntry
	if err := json.Unmarshal(body, &list); err != nil {
		return nil
	}
	var out []candidate
	seen := map[string]bool{}
	for _, e := range list {
		h := hostOf(e.URL)
		if e.Official || e.Status != "online" || e.Access == "private" || seen[h] || denied(h) {
			continue
		}
		seen[h] = true
		out = append(out, candidate{host: h, kind: f.kind, origin: f.name, online: true})
	}
	return out
}
