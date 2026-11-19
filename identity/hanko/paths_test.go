package hanko

import "testing"

func TestLocalPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/": true, "/account": true, "/a/../b?c=d": true,
		"//evil.example": false, `/\evil.example`: false, `/../\evil.example`: false,
		"/\t/evil.example": false, "/a\x00": false, "https://evil.example": false, "": false,
	} {
		if localPath(p) != want {
			t.Errorf("localPath(%q) = %v", p, !want)
		}
	}
}
