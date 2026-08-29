package qas

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
)

// ponytail: regexp2 for Python lookaround/backrefs; drop if MagicRename is rewritten without them.

type MagicRename struct {
	magicRegex      map[string]map[string]string
	taskname        string
	startI          int
	dirFilenameDict map[int]string
}

var defaultMagicRegex = map[string]map[string]string{
	"$TV": {
		"pattern": `.*?([Ss]\d{1,2})?(?:[第EePpXx\.\-\_\( ]{1,2}|^)(\d{1,3})(?!\d).*?\.(mp4|mkv)`,
		"replace": `\1E\2.\3`,
	},
	"$BLACK_WORD": {
		"pattern": `^(?!.*纯享)(?!.*加更)(?!.*超前企划)(?!.*训练室)(?!.*蒸蒸日上).*`,
		"replace": "",
	},
}

var magicVariableOrder = []struct {
	key    string
	regexs []string
}{
	{"{TASKNAME}", nil},
	{"{I}", nil},
	{"{EXT}", []string{`(?<=\.)\w+$`}},
	{"{CHINESE}", []string{`[\u4e00-\u9fa5]{2,}`}},
	{"{DATE}", []string{
		`(18|19|20)?\d{2}[\.\-/年]\d{1,2}[\.\-/月]\d{1,2}`,
		`(?<!\d)[12]\d{3}[01]?\d[0123]?\d`,
		`(?<!\d)[01]?\d[\.\-/月][0123]?\d`,
	}},
	{"{YEAR}", []string{`(?<!\d)(18|19|20)\d{2}(?!\d)`}},
	{"{S}", []string{`(?<=[Ss])\d{1,2}(?=[EeXx])`, `(?<=[Ss])\d{1,2}`}},
	{"{SXX}", []string{`[Ss]\d{1,2}(?=[EeXx])`, `[Ss]\d{1,2}`}},
	{"{E}", []string{
		`(?<=[Ss]\d\d[Ee])\d{1,3}`,
		`(?<=[Ee])\d{1,3}`,
		`(?<=[Ee][Pp])\d{1,3}`,
		`(?<=第)\d{1,3}(?=[集期话部篇])`,
		`(?<!\d)\d{1,3}(?=[集期话部篇])`,
		`(?!.*19)(?!.*20)(?<=[._])\d{1,3}(?=[._])`,
		`^\d{1,3}(?=\.\w+)`,
		`(?<!\d)\d{1,3}(?!\d)(?!$)`,
	}},
	{"{PART}", []string{`(?<=[集期话部篇第])[上中下一二三四五六七八九十]`, `[上中下一二三四五六七八九十]`}},
	{"{VER}", []string{`[\u4e00-\u9fa5]+版`}},
}

var renamePriority = []string{"上", "中", "下", "一", "二", "三", "四", "五", "六", "七", "八", "九", "十", "百", "千", "万"}

var pythonBackrefRE = regexp.MustCompile(`\\(\d+)`)

func NewMagicRename(overrides map[string]any) *MagicRename {
	magic := map[string]map[string]string{}
	for key, value := range defaultMagicRegex {
		copied := map[string]string{"pattern": value["pattern"], "replace": value["replace"]}
		magic[key] = copied
	}
	for key, raw := range overrides {
		item := mapValue(raw)
		if asString(item["pattern"]) == "" && asString(item["replace"]) == "" {
			continue
		}
		magic[key] = map[string]string{"pattern": asString(item["pattern"]), "replace": asString(item["replace"])}
	}
	return &MagicRename{magicRegex: magic, startI: 1, dirFilenameDict: map[int]string{}}
}

func (m *MagicRename) SetTaskname(name string) { m.taskname = name }

func (m *MagicRename) Conv(pattern, replace string) (string, string) {
	if spec, ok := m.magicRegex[pattern]; ok {
		pattern = spec["pattern"]
		if replace == "" {
			replace = spec["replace"]
		}
	}
	return pattern, replace
}

func normalizePythonPattern(pattern string) string {
	// Python allows \_ \( inside classes; .NET/regexp2 does not.
	replacer := strings.NewReplacer(`\_`, `_`, `\(`, `(`, `\)`, `)`)
	return replacer.Replace(pattern)
}

func compilePythonRE(pattern string) (*regexp2.Regexp, error) {
	if pattern == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	return regexp2.Compile(normalizePythonPattern(pattern), regexp2.None)
}

func pythonSearch(pattern, text string) (string, bool) {
	re, err := compilePythonRE(pattern)
	if err != nil {
		return "", false
	}
	match, err := re.FindStringMatch(text)
	if err != nil || match == nil {
		return "", false
	}
	return match.String(), true
}

func pythonMatchStart(pattern, text string) ([]string, bool) {
	re, err := compilePythonRE(pattern)
	if err != nil {
		return nil, false
	}
	match, err := re.FindStringMatch(text)
	if err != nil || match == nil || match.Index != 0 {
		return nil, false
	}
	groups := make([]string, match.GroupCount())
	for i, g := range match.Groups() {
		groups[i] = g.String()
	}
	return groups, true
}

func pythonReplaceToDotNet(replace string) string {
	return pythonBackrefRE.ReplaceAllString(replace, "$$$1")
}

func pythonSub(pattern, replace, text string) string {
	if pattern == "" {
		return replace
	}
	re, err := compilePythonRE(pattern)
	if err != nil {
		return text
	}
	result, err := re.Replace(text, pythonReplaceToDotNet(replace), -1, -1)
	if err != nil {
		return text
	}
	return result
}

func (m *MagicRename) Sub(pattern, replace, fileName string) string {
	if replace == "" {
		return fileName
	}
	for _, item := range magicVariableOrder {
		if !strings.Contains(replace, item.key) {
			continue
		}
		matched := false
		value := ""
		for _, p := range item.regexs {
			if got, ok := pythonSearch(p, fileName); ok {
				value = got
				matched = true
				if item.key == "{DATE}" {
					digits := strings.Map(func(r rune) rune {
						if r >= '0' && r <= '9' {
							return r
						}
						return -1
					}, value)
					year := fmt.Sprintf("%d", time.Now().Year())
					if len(digits) < 8 {
						digits = year[:8-len(digits)] + digits
					}
					value = digits
				}
				replace = strings.ReplaceAll(replace, item.key, value)
				break
			}
		}
		switch item.key {
		case "{TASKNAME}":
			replace = strings.ReplaceAll(replace, item.key, m.taskname)
		case "{SXX}":
			if !matched {
				replace = strings.ReplaceAll(replace, item.key, "S01")
			}
		case "{I}":
			// {I}/{II} placeholders are filled in SortFileList.
		default:
			if !matched && len(item.regexs) > 0 {
				replace = strings.ReplaceAll(replace, item.key, "")
			}
		}
	}
	if pattern != "" {
		return pythonSub(pattern, replace, fileName)
	}
	return replace
}

func customSortKey(name string) string {
	for i, keyword := range renamePriority {
		if strings.Contains(name, keyword) {
			name = strings.ReplaceAll(name, keyword, fmt.Sprintf("_%02d_", i))
		}
	}
	return name
}

func naturalLess(a, b string) bool {
	ia, ib := 0, 0
	for ia < len(a) && ib < len(b) {
		da, db := isDigit(a[ia]), isDigit(b[ib])
		if da && db {
			na, naEnd := readInt(a, ia)
			nb, nbEnd := readInt(b, ib)
			if na != nb {
				return na < nb
			}
			ia, ib = naEnd, nbEnd
			continue
		}
		if a[ia] != b[ib] {
			return a[ia] < b[ib]
		}
		ia++
		ib++
	}
	return len(a) < len(b)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func readInt(s string, i int) (int, int) {
	n := 0
	for i < len(s) && isDigit(s[i]) {
		n = n*10 + int(s[i]-'0')
		i++
	}
	return n, i
}

func (m *MagicRename) SortFileList(fileList []map[string]any) {
	type row struct {
		key string
		idx int
	}
	var names []string
	for _, f := range fileList {
		if asString(f["file_name_re"]) != "" && !asBoolDefault(f["dir"], false) {
			names = append(names, fmt.Sprintf("%s_%v", f["file_name_re"], f["updated_at"]))
		}
	}
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
	}
	for _, existing := range m.dirFilenameDict {
		if !seen[existing] {
			names = append(names, existing)
			seen[existing] = true
		}
	}
	sortWithKey(names, customSortKey)
	index := map[string]int{}
	used := map[int]bool{}
	for k := range m.dirFilenameDict {
		used[k] = true
	}
	for _, name := range names {
		skip := false
		for _, existing := range m.dirFilenameDict {
			if existing == name {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		i := indexOf(names, name) + 1
		for used[i] {
			i++
		}
		used[i] = true
		m.dirFilenameDict[i] = name
		index[name] = i
	}
	iPlus := regexp.MustCompile(`\{I+\}`)
	for _, file := range fileList {
		reName := asString(file["file_name_re"])
		if reName == "" {
			continue
		}
		if loc := iPlus.FindString(reName); loc != "" {
			key := fmt.Sprintf("%s_%v", reName, file["updated_at"])
			i := index[key]
			file["file_name_re"] = strings.Replace(reName, loc, fmt.Sprintf("%0*d", strings.Count(loc, "I"), i), 1)
		}
	}
}

func sortWithKey(names []string, key func(string) string) {
	// insertion sort keeps the port tiny and stable enough for episode lists.
	for i := 1; i < len(names); i++ {
		j := i
		for j > 0 && naturalLess(key(names[j]), key(names[j-1])) {
			names[j], names[j-1] = names[j-1], names[j]
			j--
		}
	}
}

func indexOf(items []string, target string) int {
	for i, item := range items {
		if item == target {
			return i
		}
	}
	return 0
}

func (m *MagicRename) SetDirFileList(fileList []map[string]any, replace string) {
	m.dirFilenameDict = map[int]string{}
	var names []string
	for _, f := range fileList {
		if !asBoolDefault(f["dir"], false) {
			names = append(names, asString(f["file_name"]))
		}
	}
	simpleSort(names)
	if len(names) == 0 {
		return
	}
	iPlus := regexp.MustCompile(`\{I+\}`)
	match := iPlus.FindString(replace)
	if match == "" {
		return
	}
	patternI := strings.Repeat(`\d`, strings.Count(match, "I"))
	pattern := strings.Replace(replace, match, "🔢", 1)
	for _, item := range magicVariableOrder {
		pattern = strings.ReplaceAll(pattern, item.key, "🔣")
	}
	pattern = regexp.MustCompile(`\\[0-9]+`).ReplaceAllString(pattern, "🔣")
	escaped := regexp.QuoteMeta(pattern)
	escaped = strings.ReplaceAll(escaped, "🔣", ".*?")
	escaped = strings.ReplaceAll(escaped, "🔢", ")("+patternI+")(")
	full := "(" + escaped + ")"
	if groups, ok := pythonMatchStart(full, names[len(names)-1]); ok && len(groups) >= 3 {
		if n, err := strconv.Atoi(groups[2]); err == nil {
			m.startI = n
		}
	}
	for _, filename := range names {
		if groups, ok := pythonMatchStart(full, filename); ok && len(groups) >= 4 {
			if n, err := strconv.Atoi(groups[2]); err == nil {
				m.dirFilenameDict[n] = groups[1] + match + groups[3]
			}
		}
	}
}

func simpleSort(names []string) {
	for i := 1; i < len(names); i++ {
		j := i
		for j > 0 && names[j] < names[j-1] {
			names[j], names[j-1] = names[j-1], names[j]
			j--
		}
	}
}

func (m *MagicRename) Exists(filename string, filenameList []string, ignoreExt bool) string {
	if ignoreExt {
		filename = strings.TrimSuffix(filename, path.Ext(filename))
		trimmed := make([]string, len(filenameList))
		for i, item := range filenameList {
			trimmed[i] = strings.TrimSuffix(item, path.Ext(item))
		}
		filenameList = trimmed
	}
	iPlus := regexp.MustCompile(`\{I+\}`)
	if loc := iPlus.FindString(filename); loc != "" {
		patternI := strings.Repeat(`\d`, strings.Count(loc, "I"))
		pattern := strings.ReplaceAll(regexp.QuoteMeta(filename), regexp.QuoteMeta(loc), patternI)
		for _, item := range filenameList {
			if groups, ok := pythonMatchStart(pattern, item); ok && len(groups) > 0 {
				return item
			}
		}
		return ""
	}
	for _, item := range filenameList {
		if item == filename {
			return filename
		}
	}
	return ""
}

func ApplySharePreview(data, task map[string]any, magic map[string]any, existing []map[string]any) map[string]any {
	if len(task) == 0 {
		return data
	}
	matcher := NewMagicRename(magic)
	matcher.SetTaskname(asString(task["taskname"]))
	var existingNames []string
	for _, item := range existing {
		existingNames = append(existingNames, asString(item["file_name"]))
	}
	pattern, replace := matcher.Conv(asString(task["pattern"]), asString(task["replace"]))
	list, _ := data["list"].([]any)
	if list == nil {
		for _, item := range listValue(data["list"]) {
			list = append(list, item)
		}
	}
	for _, raw := range list {
		share, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := asString(share["file_name"])
		searchPattern := pattern
		if asBoolDefault(share["dir"], false) && asString(task["update_subdir"]) != "" {
			searchPattern = asString(task["update_subdir"])
		}
		if _, ok := pythonSearch(searchPattern, name); !ok {
			continue
		}
		renamed := name
		if !asBoolDefault(share["dir"], false) {
			renamed = matcher.Sub(pattern, replace, name)
		}
		if saved := matcher.Exists(renamed, existingNames, asBoolDefault(task["ignore_extension"], false) && !asBoolDefault(share["dir"], false)); saved != "" {
			share["file_name_saved"] = saved
		} else {
			share["file_name_re"] = renamed
		}
	}
	if regexp.MustCompile(`\{I+\}`).MatchString(replace) {
		matcher.SetDirFileList(existing, replace)
		converted := make([]map[string]any, 0, len(list))
		for _, raw := range list {
			if item, ok := raw.(map[string]any); ok {
				converted = append(converted, item)
			}
		}
		matcher.SortFileList(converted)
	}
	data["list"] = list
	return data
}
