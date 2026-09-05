package e2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var releaseTestImageNames = []string{"moto", "backup", "proxy", "vaultwarden"}

func TestResolveE2EImagesAcceptsCompleteReleaseInputs(t *testing.T) {
	t.Parallel()
	const productTag = "git-0123456789abcdef0123456789abcdef01234567"

	for _, useLinks := range []bool{false, true} {
		useLinks := useLinks
		name := "regular-files"
		if useLinks {
			name = "link-farm"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			for _, image := range releaseTestImageNames {
				archive := filepath.Join(directory, image+".tar.gz")
				if useLinks {
					target := filepath.Join(t.TempDir(), image+".tar.gz")
					if err := os.WriteFile(target, []byte(image), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, archive); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.WriteFile(archive, []byte(image), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			images, err := resolveE2EImages(directory, productTag)
			if err != nil {
				t.Fatal(err)
			}
			if images.directory != directory || images.productTag != productTag {
				t.Fatalf("resolved identity = %#v", images)
			}
			if len(images.archives) != len(releaseTestImageNames) {
				t.Fatalf("archive count = %d, want %d", len(images.archives), len(releaseTestImageNames))
			}
			for _, image := range releaseTestImageNames {
				want := filepath.Join(directory, image+".tar.gz")
				if got := images.archives[image]; got != want {
					t.Fatalf("%s archive = %q, want %q", image, got, want)
				}
			}
		})
	}
}

func TestResolveE2EImagesRejectsInvalidDirectory(t *testing.T) {
	t.Parallel()
	const productTag = "git-0123456789abcdef0123456789abcdef01234567"
	validDirectory := t.TempDir()
	regularFile := filepath.Join(t.TempDir(), "images")
	if err := os.WriteFile(regularFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		directory string
		wantError string
	}{
		{name: "empty", directory: "", wantError: "clean absolute path"},
		{name: "relative", directory: "images", wantError: "clean absolute path"},
		{name: "unclean", directory: validDirectory + string(filepath.Separator) + "child" + string(filepath.Separator) + "..", wantError: "clean absolute path"},
		{name: "missing", directory: filepath.Join(t.TempDir(), "missing"), wantError: "stat image directory"},
		{name: "regular file", directory: regularFile, wantError: "not a directory"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveE2EImages(test.directory, productTag)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("resolve error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestResolveE2EImagesRejectsInvalidProductTag(t *testing.T) {
	t.Parallel()
	directory := createReleaseTestArchives(t)
	tags := []string{
		"",
		"local",
		"git-0123456789abcdef0123456789abcdef0123456",
		"git-0123456789ABCDEF0123456789ABCDEF01234567",
		"release-0123456789abcdef0123456789abcdef01234567",
		"git-0123456789abcdef0123456789abcdef01234567-extra",
	}
	for _, tag := range tags {
		tag := tag
		t.Run(tag, func(t *testing.T) {
			t.Parallel()
			_, err := resolveE2EImages(directory, tag)
			if err == nil || !strings.Contains(err.Error(), "complete lowercase commit") {
				t.Fatalf("resolve error = %v", err)
			}
		})
	}
}

func TestResolveE2EImagesRequiresEveryRegularArchive(t *testing.T) {
	t.Parallel()
	const productTag = "git-0123456789abcdef0123456789abcdef01234567"
	for _, image := range releaseTestImageNames {
		image := image
		t.Run("missing-"+image, func(t *testing.T) {
			t.Parallel()
			directory := createReleaseTestArchives(t)
			if err := os.Remove(filepath.Join(directory, image+".tar.gz")); err != nil {
				t.Fatal(err)
			}
			_, err := resolveE2EImages(directory, productTag)
			if err == nil || !strings.Contains(err.Error(), "stat "+image+" image archive") {
				t.Fatalf("resolve error = %v", err)
			}
		})
	}

	t.Run("non-regular", func(t *testing.T) {
		t.Parallel()
		directory := createReleaseTestArchives(t)
		archive := filepath.Join(directory, "moto.tar.gz")
		if err := os.Remove(archive); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(archive, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := resolveE2EImages(directory, productTag)
		if err == nil || !strings.Contains(err.Error(), "moto image archive is not a regular file") {
			t.Fatalf("resolve error = %v", err)
		}
	})
}

func createReleaseTestArchives(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	for _, image := range releaseTestImageNames {
		if err := os.WriteFile(filepath.Join(directory, image+".tar.gz"), []byte(image), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}
