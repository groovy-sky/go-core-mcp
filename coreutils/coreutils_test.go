package coreutils

import "testing"

func TestHeadAndTailAreBounded(t *testing.T) {
	text := "one\ntwo\nthree\nfour\n"

	head, truncated := Head(text, 2)
	if head != "one\ntwo\n" || !truncated {
		t.Fatalf("unexpected head result %q truncated=%t", head, truncated)
	}

	tail, truncated := Tail(text, 2)
	if tail != "three\nfour\n" || !truncated {
		t.Fatalf("unexpected tail result %q truncated=%t", tail, truncated)
	}

	head, truncated = Head(text, 10)
	if head != text || truncated {
		t.Fatalf("short files must not be truncated, got %q truncated=%t", head, truncated)
	}
}

func TestClampLineEnforcesMaxLineLength(t *testing.T) {
	long := make([]byte, MaxLineLength+10)
	for index := range long {
		long[index] = 'a'
	}
	clamped, truncated := ClampLine(string(long))
	if !truncated || len(clamped) != MaxLineLength {
		t.Fatalf("expected clamped line of %d bytes, got %d truncated=%t", MaxLineLength, len(clamped), truncated)
	}
}

func TestWordCount(t *testing.T) {
	counts := WordCount("alpha beta\ngamma\n")
	if counts.Lines != 2 || counts.Words != 3 || counts.Bytes != 17 {
		t.Fatalf("unexpected counts %+v", counts)
	}
}

func TestSortAndUniq(t *testing.T) {
	if got := Sort("b\na\nb\n", false, false, true); got != "a\nb\n" {
		t.Fatalf("unexpected sort result %q", got)
	}
	if got := Sort("2\n10\n", false, true, false); got != "2\n10\n" {
		t.Fatalf("unexpected numeric sort result %q", got)
	}
	if got := Uniq("a\na\nb\n", false); got != "a\nb\n" {
		t.Fatalf("unexpected uniq result %q", got)
	}
	if got := Uniq("a\na\n", true); got != "      2 a\n" {
		t.Fatalf("unexpected counted uniq result %q", got)
	}
}

func TestCutAndPaste(t *testing.T) {
	got, err := Cut("a:b:c\nd:e:f\n", ":", []int{1, 3})
	if err != nil {
		t.Fatalf("cut failed: %v", err)
	}
	if got != "a:c\nd:f\n" {
		t.Fatalf("unexpected cut result %q", got)
	}
	if _, err := Cut("a\n", ":", []int{0}); err == nil {
		t.Fatal("expected 1-based field validation to fail")
	}

	pasted, err := Paste([]string{"a\nb\n", "1\n"}, ",")
	if err != nil {
		t.Fatalf("paste failed: %v", err)
	}
	if pasted != "a,1\nb,\n" {
		t.Fatalf("unexpected paste result %q", pasted)
	}
}

func TestTrAndBase64(t *testing.T) {
	translated, err := Tr("abc", "ab", "xy", false)
	if err != nil || translated != "xyc" {
		t.Fatalf("unexpected tr result %q err=%v", translated, err)
	}
	deleted, err := Tr("abc", "b", "", true)
	if err != nil || deleted != "ac" {
		t.Fatalf("unexpected tr delete result %q err=%v", deleted, err)
	}

	encoded := Base64Encode("hi")
	decoded, err := Base64Decode(encoded)
	if err != nil || decoded != "hi" {
		t.Fatalf("round trip failed: %q err=%v", decoded, err)
	}
	if _, err := Base64Decode("not base64!!"); err == nil {
		t.Fatal("expected invalid base64 to be rejected")
	}
	if _, err := Base64Decode(Base64Encode(string([]byte{0xff, 0xfe}))); err == nil {
		t.Fatal("expected non-UTF-8 decoded data to be rejected")
	}
}

func TestGrepLimitsMatches(t *testing.T) {
	text := "TODO one\nTODO two\nTODO three\nother\n"
	matches, truncated, err := Grep(text, GrepOptions{Pattern: "TODO", MaxMatches: 2})
	if err != nil {
		t.Fatalf("grep failed: %v", err)
	}
	if len(matches) != 2 || !truncated {
		t.Fatalf("expected 2 truncated matches, got %d truncated=%t", len(matches), truncated)
	}
	if matches[0].Line != 1 || matches[0].Text != "TODO one" {
		t.Fatalf("unexpected first match %+v", matches[0])
	}
	if _, _, err := Grep(text, GrepOptions{Pattern: "("}); err == nil {
		t.Fatal("expected invalid pattern to be rejected")
	}
	matches, _, err = Grep(text, GrepOptions{Pattern: "todo", IgnoreCase: true, FixedText: true, MaxMatches: 20})
	if err != nil || len(matches) != 3 {
		t.Fatalf("unexpected fixed/ignore-case result %d err=%v", len(matches), err)
	}
}

func TestCompileMatcherSupportsFixedAndRegexModes(t *testing.T) {
	matcher, err := CompileMatcher("todo", true, true)
	if err != nil {
		t.Fatalf("CompileMatcher fixed: %v", err)
	}
	if !matcher("TODO item") || matcher("done") {
		t.Fatal("fixed matcher produced unexpected results")
	}

	regex, err := CompileMatcher("^a.+z$", false, false)
	if err != nil {
		t.Fatalf("CompileMatcher regex: %v", err)
	}
	if !regex("abcz") || regex("ab") {
		t.Fatal("regex matcher produced unexpected results")
	}
}

func TestBasenameAndDirname(t *testing.T) {
	if got := Basename("docs/README.md", ".md"); got != "README" {
		t.Fatalf("unexpected basename %q", got)
	}
	if got := Basename("docs/", ""); got != "docs" {
		t.Fatalf("unexpected basename %q", got)
	}
	if got := Dirname("docs/README.md"); got != "docs" {
		t.Fatalf("unexpected dirname %q", got)
	}
}

func TestSha256Sum(t *testing.T) {
	if got := Sha256Sum([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("unexpected digest %q", got)
	}
}
