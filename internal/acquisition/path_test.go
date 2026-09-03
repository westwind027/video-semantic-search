package acquisition

import "testing"

func TestNormalizeLocalPathConvertsWindowsDrivePaths(t *testing.T) {
	tests := map[string]string{
		`E:\Movies\movie.mp4`: "/mnt/e/Movies/movie.mp4",
		`C:/Movies/movie.mp4`: "/mnt/c/Movies/movie.mp4",
	}
	for input, want := range tests {
		if got := NormalizeLocalPath(input); got != want {
			t.Fatalf("NormalizeLocalPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeLocalPathKeepsLinuxPaths(t *testing.T) {
	input := "/mnt/e/Movies/movie.mp4"
	if got := NormalizeLocalPath(input); got != input {
		t.Fatalf("NormalizeLocalPath(%q) = %q, want %q", input, got, input)
	}
}
