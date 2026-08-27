// Copyright 2025 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// This code is based on or derived from doublestar
// Copyright (c) 2014 Bob Matcuk
// Licensed under MIT License
// https://github.com/bmatcuk/doublestar/blob/master/LICENSE

package glob

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	globutil "github.com/bmatcuk/doublestar/v4"
)

type MatchTest struct {
	pattern, testPath     string
	shouldMatch           bool
	shouldMatchGlob       bool
	expectedErr           error
	expectIOErr           bool
	expectPatternNotExist bool
	isStandard            bool
	testOnDisk            bool
	numResults            int
	winNumResults         int
}

// Tests which contain escapes and symlinks will not work on Windows
var onWindows = runtime.GOOS == "windows"

var matchTests = []MatchTest{
	{"", "", true, false, nil, true, false, true, true, 0, 0},
	{"*", "", true, true, nil, false, false, true, false, 0, 0},
	{"*", "/", false, false, nil, false, false, true, false, 0, 0},
	{"/*", "/", true, true, nil, false, false, true, false, 0, 0},
	{"/*", "/debug/", false, false, nil, false, false, true, false, 0, 0},
	{"/*", "//", false, false, nil, false, false, true, false, 0, 0},
	{"abc", "abc", true, true, nil, false, false, true, true, 1, 1},
	{"*", "abc", true, true, nil, false, false, true, true, 22, 17},
	{"*c", "abc", true, true, nil, false, false, true, true, 2, 2},
	{"*/", "a/", true, true, nil, false, false, true, false, 0, 0},
	{"a*", "a", true, true, nil, false, false, true, true, 9, 9},
	{"a*", "abc", true, true, nil, false, false, true, true, 9, 9},
	{"a*", "ab/c", false, false, nil, false, false, true, true, 9, 9},
	{"a*/b", "abc/b", true, true, nil, false, false, true, true, 2, 2},
	{"a*/b", "a/c/b", false, false, nil, false, false, true, true, 2, 2},
	{"a*/c/", "a/b", false, false, nil, false, false, false, true, 1, 1},
	{"a*b*c*d*e*", "axbxcxdxe", true, true, nil, false, false, true, true, 3, 3},
	{"a*b*c*d*e*/f", "axbxcxdxe/f", true, true, nil, false, false, true, true, 2, 2},
	{"a*b*c*d*e*/f", "axbxcxdxexxx/f", true, true, nil, false, false, true, true, 2, 2},
	{"a*b*c*d*e*/f", "axbxcxdxe/xxx/f", false, false, nil, false, false, true, true, 2, 2},
	{"a*b*c*d*e*/f", "axbxcxdxexxx/fff", false, false, nil, false, false, true, true, 2, 2},
	{"a*b?c*x", "abxbbxdbxebxczzx", true, true, nil, false, false, true, true, 2, 2},
	{"a*b?c*x", "abxbbxdbxebxczzy", false, false, nil, false, false, true, true, 2, 2},
	{"ab[c]", "abc", true, true, nil, false, false, true, true, 1, 1},
	{"ab[b-d]", "abc", true, true, nil, false, false, true, true, 1, 1},
	{"ab[e-g]", "abc", false, false, nil, false, false, true, true, 0, 0},
	{"ab[^c]", "abc", false, false, nil, false, false, true, true, 0, 0},
	{"ab[^b-d]", "abc", false, false, nil, false, false, true, true, 0, 0},
	{"ab[^e-g]", "abc", true, true, nil, false, false, true, true, 1, 1},
	{"a\\*b", "ab", false, false, nil, false, true, true, !onWindows, 0, 0},
	{"a?b", "a☺b", true, true, nil, false, false, true, true, 1, 1},
	{"a[^a]b", "a☺b", true, true, nil, false, false, true, true, 1, 1},
	{"a[!a]b", "a☺b", true, true, nil, false, false, false, true, 1, 1},
	{"a???b", "a☺b", false, false, nil, false, false, true, true, 0, 0},
	{"a[^a][^a][^a]b", "a☺b", false, false, nil, false, false, true, true, 0, 0},
	{"[a-ζ]*", "α", true, true, nil, false, false, true, true, 20, 17},
	{"*[a-ζ]", "A", false, false, nil, false, false, true, true, 20, 17},
	{"a?b", "a/b", false, false, nil, false, false, true, true, 1, 1},
	{"a*b", "a/b", false, false, nil, false, false, true, true, 1, 1},
	{"[\\]a]", "]", true, true, nil, false, false, true, !onWindows, 2, 2},
	{"[\\-]", "-", true, true, nil, false, false, true, !onWindows, 1, 1},
	{"[x\\-]", "x", true, true, nil, false, false, true, !onWindows, 2, 2},
	{"[x\\-]", "-", true, true, nil, false, false, true, !onWindows, 2, 2},
	{"[x\\-]", "z", false, false, nil, false, false, true, !onWindows, 2, 2},
	{"[\\-x]", "x", true, true, nil, false, false, true, !onWindows, 2, 2},
	{"[\\-x]", "-", true, true, nil, false, false, true, !onWindows, 2, 2},
	{"[\\-x]", "a", false, false, nil, false, false, true, !onWindows, 2, 2},
	{"[]a]", "]", false, false, globutil.ErrBadPattern, false, false, true, true, 0, 0},
	// doublestar, like bash, allows these when path.Match() does not
	{"[-]", "-", true, true, nil, false, false, false, !onWindows, 1, 0},
	{"[x-]", "x", true, true, nil, false, false, false, true, 2, 1},
	{"[x-]", "-", true, true, nil, false, false, false, !onWindows, 2, 1},
	{"[x-]", "z", false, false, nil, false, false, false, true, 2, 1},
	{"[-x]", "x", true, true, nil, false, false, false, true, 2, 1},
	{"[-x]", "-", true, true, nil, false, false, false, !onWindows, 2, 1},
	{"[-x]", "a", false, false, nil, false, false, false, true, 2, 1},
	{"[a-b-d]", "a", true, true, nil, false, false, false, true, 3, 2},
	{"[a-b-d]", "b", true, true, nil, false, false, false, true, 3, 2},
	{"[a-b-d]", "-", true, true, nil, false, false, false, !onWindows, 3, 2},
	{"[a-b-d]", "c", false, false, nil, false, false, false, true, 3, 2},
	{"[a-b-x]", "x", true, true, nil, false, false, false, true, 4, 3},
	{"\\", "a", false, false, globutil.ErrBadPattern, false, false, true, !onWindows, 0, 0},
	{"[", "a", false, false, globutil.ErrBadPattern, false, false, true, true, 0, 0},
	{"[^", "a", false, false, globutil.ErrBadPattern, false, false, true, true, 0, 0},
	{"[^bc", "a", false, false, globutil.ErrBadPattern, false, false, true, true, 0, 0},
	{"a[", "a", false, false, globutil.ErrBadPattern, false, false, true, true, 0, 0},
	{"a[", "ab", false, false, globutil.ErrBadPattern, false, false, true, true, 0, 0},
	{"ad[", "ab", false, false, globutil.ErrBadPattern, false, false, true, true, 0, 0},
	{"*x", "xxx", true, true, nil, false, false, true, true, 4, 4},
	{"[abc]", "b", true, true, nil, false, false, true, true, 3, 3},
	{"**", "", true, true, nil, false, false, false, false, 38, 38},
	{"a/**", "a", true, false, nil, false, false, false, true, 7, 7},
	{"a/**", "a/", true, true, nil, false, false, false, false, 7, 7},
	{"a/**/", "a/", true, true, nil, false, false, false, false, 4, 4},
	{"a/**", "a/b", true, true, nil, false, false, false, true, 7, 7},
	{"a/**", "a/b/c", true, true, nil, false, false, false, true, 7, 7},
	{"**/c", "c", true, true, nil, !onWindows, false, false, true, 5, 4},
	{"**/c", "b/c", true, true, nil, !onWindows, false, false, true, 5, 4},
	{"**/c", "a/b/c", true, true, nil, !onWindows, false, false, true, 5, 4},
	{"**/c", "a/b", false, false, nil, !onWindows, false, false, true, 5, 4},
	{"**/c", "abcd", false, false, nil, !onWindows, false, false, true, 5, 4},
	{"**/c", "a/abc", false, false, nil, !onWindows, false, false, true, 5, 4},
	{"a/**/b", "a/b", true, true, nil, false, false, false, true, 2, 2},
	{"a/**/c", "a/b/c", true, true, nil, false, false, false, true, 2, 2},
	{"a/**/d", "a/b/c/d", true, true, nil, false, false, false, true, 1, 1},
	{"a/\\**", "a/b/c", false, false, nil, false, false, false, !onWindows, 0, 0},
	{"a/\\[*\\]", "a/bc", false, false, nil, false, false, true, !onWindows, 0, 0},
	// this fails the FilepathGlob test on Windows
	{"a/b/c", "a/b//c", false, false, nil, false, false, true, !onWindows, 1, 1},
	// odd: Glob + filepath.Glob return results
	{"a/", "a", false, false, nil, false, false, true, false, 0, 0},
	{"ab{c,d}", "abc", true, true, nil, false, true, false, true, 1, 1},
	{"ab{c,d,*}", "abcde", true, true, nil, false, true, false, true, 5, 5},
	{"ab{c,d}[", "abcd", false, false, globutil.ErrBadPattern, false, false, false, true, 0, 0},
	{"a{,bc}", "a", true, true, nil, false, false, false, true, 2, 2},
	{"a{,bc}", "abc", true, true, nil, false, false, false, true, 2, 2},
	{"a/{b/c,c/b}", "a/b/c", true, true, nil, false, false, false, true, 2, 2},
	{"a/{b/c,c/b}", "a/c/b", true, true, nil, false, false, false, true, 2, 2},
	{"a/a*{b,c}", "a/abc", true, true, nil, false, false, false, true, 1, 1},
	{"{a/{b,c},abc}", "a/b", true, true, nil, false, false, false, true, 3, 3},
	{"{a/{b,c},abc}", "a/c", true, true, nil, false, false, false, true, 3, 3},
	{"{a/{b,c},abc}", "abc", true, true, nil, false, false, false, true, 3, 3},
	{"{a/{b,c},abc}", "a/b/c", false, false, nil, false, false, false, true, 3, 3},
	{"{a/ab*}", "a/abc", true, true, nil, false, false, false, true, 1, 1},
	{"{a/*}", "a/b", true, true, nil, false, false, false, true, 3, 3},
	{"{a/abc}", "a/abc", true, true, nil, false, false, false, true, 1, 1},
	{"{a/b,a/c}", "a/c", true, true, nil, false, false, false, true, 2, 2},
	{"abc/**", "abc/b", true, true, nil, false, false, false, true, 3, 3},
	{"**/abc", "abc", true, true, nil, !onWindows, false, false, true, 2, 2},
	{"abc**", "abc/b", false, false, nil, false, false, false, true, 3, 3},
	{"**/*.txt", "abc/ßtestß.txt", true, true, nil, !onWindows, false, false, true, 1, 1},
	{"**/ß*", "abc/ßtestß.txt", true, true, nil, !onWindows, false, false, true, 1, 1},
	{"**/{a,b}", "a/b", true, true, nil, !onWindows, false, false, true, 5, 5},
	// unfortunately, io/fs can't handle this, so neither can Glob =(
	{"broken-symlink", "broken-symlink", true, true, nil, false, false, true, false, 1, 1},
	{"broken-symlink/*", "a", false, false, nil, false, true, true, true, 0, 0},
	{"broken*/*", "a", false, false, nil, false, false, true, true, 0, 0},
	{"working-symlink/c/*", "working-symlink/c/d", true, true, nil, false, false, true, !onWindows, 1, 1},
	{"working-sym*/*", "working-symlink/c", true, true, nil, false, false, true, !onWindows, 1, 1},
	{"b/**/f", "b/symlink-dir/f", true, true, nil, false, false, false, !onWindows, 2, 2},
	{"*/symlink-dir/*", "b/symlink-dir/f", true, true, nil, !onWindows, false, true, !onWindows, 2, 2},
	{"e/**", "e/**", true, true, nil, false, false, false, !onWindows, 11, 6},
	{"e/**", "e/*", true, true, nil, false, false, false, !onWindows, 11, 6},
	{"e/**", "e/?", true, true, nil, false, false, false, !onWindows, 11, 6},
	{"e/**", "e/[", true, true, nil, false, false, false, true, 11, 6},
	{"e/**", "e/]", true, true, nil, false, false, false, true, 11, 6},
	{"e/**", "e/[]", true, true, nil, false, false, false, true, 11, 6},
	{"e/**", "e/{", true, true, nil, false, false, false, true, 11, 6},
	{"e/**", "e/}", true, true, nil, false, false, false, true, 11, 6},
	{"e/**", "e/\\", true, true, nil, false, false, false, !onWindows, 11, 6},
	{"e/*", "e/*", true, true, nil, false, false, true, !onWindows, 10, 5},
	{"e/?", "e/?", true, true, nil, false, false, true, !onWindows, 7, 4},
	{"e/?", "e/*", true, true, nil, false, false, true, !onWindows, 7, 4},
	{"e/?", "e/[", true, true, nil, false, false, true, true, 7, 4},
	{"e/?", "e/]", true, true, nil, false, false, true, true, 7, 4},
	{"e/?", "e/{", true, true, nil, false, false, true, true, 7, 4},
	{"e/?", "e/}", true, true, nil, false, false, true, true, 7, 4},
	{"e/\\[", "e/[", true, true, nil, false, false, true, !onWindows, 1, 1},
	{"e/[", "e/[", false, false, globutil.ErrBadPattern, false, false, true, true, 0, 0},
	{"e/]", "e/]", true, true, nil, false, false, true, true, 1, 1},
	{"e/\\]", "e/]", true, true, nil, false, false, true, !onWindows, 1, 1},
	{"e/\\{", "e/{", true, true, nil, false, false, true, !onWindows, 1, 1},
	{"e/\\}", "e/}", true, true, nil, false, false, true, !onWindows, 1, 1},
	{"e/[\\*\\?]", "e/*", true, true, nil, false, false, true, !onWindows, 2, 2},
	{"e/[\\*\\?]", "e/?", true, true, nil, false, false, true, !onWindows, 2, 2},
	{"e/[\\*\\?]", "e/**", false, false, nil, false, false, true, !onWindows, 2, 2},
	{"e/[\\*\\?]?", "e/**", true, true, nil, false, false, true, !onWindows, 1, 1},
	{"e/{\\*,\\?}", "e/*", true, true, nil, false, false, false, !onWindows, 2, 2},
	{"e/{\\*,\\?}", "e/?", true, true, nil, false, false, false, !onWindows, 2, 2},
	{"e/\\*", "e/*", true, true, nil, false, false, true, !onWindows, 1, 1},
	{"e/\\?", "e/?", true, true, nil, false, false, true, !onWindows, 1, 1},
	{"e/\\?", "e/**", false, false, nil, false, false, true, !onWindows, 1, 1},
	{"*\\}", "}", true, true, nil, false, false, true, !onWindows, 1, 1},
	{"nonexistent-path", "a", false, false, nil, false, true, true, true, 0, 0},
	{"nonexistent-path/", "a", false, false, nil, false, true, true, true, 0, 0},
	{"nonexistent-path/file", "a", false, false, nil, false, true, true, true, 0, 0},
	{"nonexistent-path/*", "a", false, false, nil, false, true, true, true, 0, 0},
	{"nonexistent-path/**", "a", false, false, nil, false, true, true, true, 0, 0},
	{"nopermission/*", "nopermission/file", true, false, nil, true, false, true, !onWindows, 0, 0},
	{"nopermission/dir/", "nopermission/dir", false, false, nil, true, false, true, !onWindows, 0, 0},
	{"nopermission/file", "nopermission/file", true, false, nil, true, false, true, !onWindows, 0, 0},
	{"node_modules/!(.cache)/**", "node_modules/others/file.txt", true, true, nil, false, false, false, !onWindows, 0, 0},
	{"node_modules/!(.cache)/**", "node_modules/.cache/file.txt", false, false, nil, false, false, false, !onWindows, 0, 0},
	{"node_modules/!(.cache)/**", "node_modules/file.txt", true, false, nil, false, false, false, !onWindows, 0, 0},
	{"node_modules/!(.cache)/**", "node_modules/others/others/file.txt", true, true, nil, false, false, false, !onWindows, 0, 0},
}

func TestValidatePattern(t *testing.T) {
	for idx, tt := range matchTests {
		testValidatePatternWith(t, idx, tt)
	}
}

func testValidatePatternWith(t *testing.T, idx int, tt MatchTest) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("#%v. Validate(%#q) panicked: %#v", idx, tt.pattern, r)
		}
	}()

	result := isValidPattern(tt.pattern, '/')
	if result != (tt.expectedErr == nil) {
		t.Errorf("#%v. ValidatePattern(%#q) = %v want %v", idx, tt.pattern, result, !result)
	}
}

func TestPathMatch(t *testing.T) {
	for idx, tt := range matchTests {
		// Even though we aren't actually matching paths on disk, we are using
		if tt.testOnDisk {
			testPathMatchWith(t, idx, tt)
		}
	}
}

func testPathMatchWith(t *testing.T, idx int, tt MatchTest) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("#%v. Match(%#q, %#q) panicked: %#v", idx, tt.pattern, tt.testPath, r)
		}
	}()

	pattern := filepath.FromSlash(tt.pattern)
	testPath := filepath.FromSlash(tt.testPath)
	ok, err := PathMatch(pattern, testPath)
	if ok != tt.shouldMatch || err != tt.expectedErr {
		t.Errorf("#%v. PathMatch(%#q, %#q) = %v, %v want %v, %v", idx, pattern, testPath, ok, err, tt.shouldMatch, tt.expectedErr)
	}

	if tt.isStandard {
		stdOk, stdErr := filepath.Match(pattern, testPath)
		if ok != stdOk || !compareErrors(err, stdErr) {
			t.Errorf("#%v. PathMatch(%#q, %#q) != filepath.Match(...). Got %v, %v want %v, %v", idx, pattern, testPath, ok, err, stdOk, stdErr)
		}
	}
}

func TestPathMatchFake(t *testing.T) {
	// This test fakes that our path separator is `\\` so we can test what it
	if onWindows {
		return
	}

	for idx, tt := range matchTests {
		// Even though we aren't actually matching paths on disk, we are using
		if tt.testOnDisk && !strings.Contains(tt.pattern, "\\") {
			testPathMatchFakeWith(t, idx, tt)
		}
	}
}

func testPathMatchFakeWith(t *testing.T, idx int, tt MatchTest) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("#%v. Match(%#q, %#q) panicked: %#v", idx, tt.pattern, tt.testPath, r)
		}
	}()

	pattern := strings.ReplaceAll(tt.pattern, "/", "\\")
	testPath := strings.ReplaceAll(tt.testPath, "/", "\\")
	ok, err := matchWithSeparator(pattern, testPath, '\\', true)
	if ok != tt.shouldMatch || err != tt.expectedErr {
		t.Errorf("#%v. PathMatch(%#q, %#q) = %v, %v want %v, %v", idx, pattern, testPath, ok, err, tt.shouldMatch, tt.expectedErr)
	}
}

func compareErrors(a, b error) bool {
	if a == nil {
		return b == nil
	}
	return b != nil
}
