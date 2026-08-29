package qas

import "testing"

func TestMagicRenameTVAndTaskname(t *testing.T) {
	mr := NewMagicRename(nil)
	mr.SetTaskname("庆余年")
	pattern, replace := mr.Conv("$TV", "")
	if got := mr.Sub(pattern, replace, "庆余年.S01E02.1080p.mkv"); got != "S01E02.mkv" {
		t.Fatalf("S01E02: %q", got)
	}
	if got := mr.Sub(pattern, replace, "庆余年 第03集.mp4"); got != "E03.mp4" {
		t.Fatalf("第03集: %q", got)
	}
	if got := mr.Sub(pattern, replace, "Name.S02E07.mkv"); got != "S02E07.mkv" {
		t.Fatalf("S02E07: %q", got)
	}
	if got := mr.Sub("", "{TASKNAME}.mkv", "whatever.mkv"); got != "庆余年.mkv" {
		t.Fatalf("taskname: %q", got)
	}
	if got := mr.Sub("", "{SXX}E{E}.{EXT}", "foo.S03E12.1080p.mkv"); got != "S03E12.mkv" {
		t.Fatalf("SXX/E/EXT: %q", got)
	}
	if got := mr.Sub(`(\d+)`, `E\1.mkv`, "12.mp4"); got != "E12.mkv.mpE4.mkv" {
		t.Fatalf("backref all: %q", got)
	}
}

func TestMagicRenameBlackWordAndExists(t *testing.T) {
	mr := NewMagicRename(nil)
	pattern, _ := mr.Conv("$BLACK_WORD", "")
	if _, ok := pythonSearch(pattern, "纯享版.mkv"); ok {
		t.Fatal("纯享 should be filtered")
	}
	if _, ok := pythonSearch(pattern, "正片.mkv"); !ok {
		t.Fatal("正片 should pass")
	}
	if got := mr.Exists("S01E{II}.mkv", []string{"S01E01.mkv", "S01E02.mkv"}, false); got != "S01E01.mkv" {
		t.Fatalf("exists II: %q", got)
	}
	if got := mr.Exists("foo.mkv", []string{"foo.mp4"}, true); got != "foo" {
		t.Fatalf("ignore ext: %q", got)
	}
}

func TestMagicRenameSequenceNumbers(t *testing.T) {
	mr := NewMagicRename(nil)
	existing := []map[string]any{
		{"file_name": "S01E01.mkv", "dir": false, "updated_at": 1},
		{"file_name": "S01E02.mkv", "dir": false, "updated_at": 2},
	}
	need := []map[string]any{
		{"file_name": "x.mkv", "file_name_re": "S01E{II}.mkv", "dir": false, "updated_at": 9},
		{"file_name": "y.mkv", "file_name_re": "S01E{II}.mkv", "dir": false, "updated_at": 10},
	}
	mr.SetDirFileList(existing, "S01E{II}.mkv")
	mr.SortFileList(need)
	if asString(need[0]["file_name_re"]) != "S01E03.mkv" || asString(need[1]["file_name_re"]) != "S01E04.mkv" {
		t.Fatalf("sequence: %#v %#v", need[0]["file_name_re"], need[1]["file_name_re"])
	}
}
