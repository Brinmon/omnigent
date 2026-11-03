package server

import (
	"path/filepath"
	"testing"
)

// The route strips the leading slash from {path:path}. A POSIX path needs it
// back; a Windows drive path is already absolute and must not gain one -- doing
// so produced "/C:/Users/me", which the picker reported as a directory that does
// not exist on the host, for the user's own home.
func TestRestoreAbsolutePath(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"posix nested", "home/user/projects", filepath.FromSlash("/home/user/projects")},
		{"posix already slashed", "/home/user", filepath.FromSlash("/home/user")},
		{"windows drive", "C:/Users/me", filepath.FromSlash("C:/Users/me")},
		{"windows lower drive", "d:/work", filepath.FromSlash("d:/work")},
		{"unc share", "//server/share/folder", filepath.FromSlash("//server/share/folder")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := restoreAbsolutePath(testCase.input); got != testCase.want {
				t.Fatalf("restoreAbsolutePath(%q) = %q, want %q",
					testCase.input, got, testCase.want)
			}
		})
	}
}

// A Windows path must never gain a leading separator: the picker sends the drive
// form, and prefixing a slash makes every listing fail with "does not exist".
func TestWindowsPathsKeepTheirRoot(t *testing.T) {
	for _, input := range []string{"C:/Users/me", "C:/", "d:/work"} {
		got := restoreAbsolutePath(input)
		if filepath.VolumeName(got) == "" {
			t.Errorf("restoreAbsolutePath(%q) = %q, which lost its drive letter",
				input, got)
		}
	}
}
