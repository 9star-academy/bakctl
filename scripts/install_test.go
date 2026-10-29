package scripts

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type installFixture struct {
	bash       string
	script     string
	root       string
	binDir     string
	installDir string
	fixtures   string
	logPath    string
	asset      string
	payload    []byte
	env        map[string]string
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("installer supports Linux and macOS")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("installer requires bash")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Skip("installer requires sha256sum or shasum")
		}
	}
	script, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	f := &installFixture{
		bash:       bash,
		script:     script,
		root:       root,
		binDir:     filepath.Join(root, "commands"),
		installDir: filepath.Join(root, "installation with spaces"),
		fixtures:   filepath.Join(root, "fixtures"),
		logPath:    filepath.Join(root, "curl.log"),
		asset:      "bakctl-linux-amd64",
		payload:    []byte("#!/bin/sh\nprintf 'verified fixture binary\\n'\n"),
	}
	for _, dir := range []string{f.binDir, f.installDir, f.fixtures, filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(f.fixtures, "binary"), f.payload, 0600)
	writeFixture(t, filepath.Join(f.fixtures, "latest.json"), []byte(`{"tag_name":"v9.8.7"}`), 0600)
	f.writeChecksums(t)
	writeFixture(t, filepath.Join(f.binDir, "uname"), []byte(`#!/bin/sh
case "$1" in
  -s) printf '%s\n' "$BAKCTL_TEST_OS" ;;
  -m) printf '%s\n' "$BAKCTL_TEST_ARCH" ;;
  *) exit 2 ;;
esac
`), 0700)
	writeFixture(t, filepath.Join(f.binDir, "sudo"), []byte("#!/bin/sh\necho 'test refuses sudo' >&2\nexit 99\n"), 0700)
	writeFixture(t, filepath.Join(f.binDir, "curl"), []byte(`#!/bin/sh
set -eu
url=''
out=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) shift; out="$1" ;;
    -fsSL) ;;
    https://*) url="$1" ;;
    *) echo "unexpected curl argument: $1" >&2; exit 90 ;;
  esac
  shift
done
printf '%s\n' "$url" >> "$BAKCTL_TEST_CURL_LOG"
case "$url" in
  https://api.github.com/repos/9star-academy/bakctl/releases/latest)
    [ "$BAKCTL_TEST_MODE" != latest_error ] || exit 22
    cat "$BAKCTL_TEST_FIXTURES/latest.json"
    ;;
  https://github.com/9star-academy/bakctl/releases/download/*/SHA256SUMS)
    [ "$BAKCTL_TEST_MODE" != missing_checksum ] || exit 22
    [ "$BAKCTL_TEST_MODE" != checksum_network_error ] || exit 56
    [ -n "$out" ] || exit 91
    cp "$BAKCTL_TEST_FIXTURES/SHA256SUMS" "$out"
    ;;
  https://github.com/9star-academy/bakctl/releases/download/*/bakctl-*)
    [ "$BAKCTL_TEST_MODE" != binary_network_error ] || exit 56
    [ -n "$out" ] || exit 91
    cp "$BAKCTL_TEST_FIXTURES/binary" "$out"
    ;;
  *) echo "unexpected URL: $url" >&2; exit 92 ;;
esac
`), 0700)
	f.env = map[string]string{
		"PATH":                 f.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMPDIR":               filepath.Join(root, "tmp"),
		"BAKCTL_INSTALL_DIR":   f.installDir,
		"BAKCTL_VERSION":       "v9.8.7",
		"BAKCTL_TEST_OS":       "Linux",
		"BAKCTL_TEST_ARCH":     "x86_64",
		"BAKCTL_TEST_MODE":     "success",
		"BAKCTL_TEST_CURL_LOG": f.logPath,
		"BAKCTL_TEST_FIXTURES": f.fixtures,
	}
	return f
}

func writeFixture(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func (f *installFixture) writeChecksums(t *testing.T) {
	t.Helper()
	// Include an unrelated asset first: the installer must select the current
	// platform's exact filename, not just the first digest in the manifest.
	manifest := fmt.Sprintf("%064d  bakctl-other-platform\n%x  %s\n", 0, sha256.Sum256(f.payload), f.asset)
	writeFixture(t, filepath.Join(f.fixtures, "SHA256SUMS"), []byte(manifest), 0600)
}

func (f *installFixture) run(t *testing.T) (string, error) {
	t.Helper()
	cmd := exec.Command(f.bash, f.script)
	cmd.Dir = f.root
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := f.env[name]; !replaced {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	for key, value := range f.env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (f *installFixture) requests(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *installFixture) assertTemporaryFilesRemoved(t *testing.T) {
	t.Helper()
	for _, dir := range []string{filepath.Join(f.root, "tmp"), f.installDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if dir == f.installDir && entry.Name() == "bakctl" {
				continue
			}
			t.Errorf("installer left temporary file: %s", filepath.Join(dir, entry.Name()))
		}
	}
}

func TestInstallVerifiedBinary(t *testing.T) {
	for _, platform := range []struct{ os, arch, asset string }{
		{"Linux", "x86_64", "bakctl-linux-amd64"},
		{"Linux", "aarch64", "bakctl-linux-arm64"},
		{"Darwin", "x86_64", "bakctl-darwin-amd64"},
		{"Darwin", "arm64", "bakctl-darwin-arm64"},
	} {
		t.Run(platform.asset, func(t *testing.T) {
			f := newInstallFixture(t)
			f.env["BAKCTL_TEST_OS"], f.env["BAKCTL_TEST_ARCH"] = platform.os, platform.arch
			f.asset = platform.asset
			f.writeChecksums(t)
			writeFixture(t, filepath.Join(f.installDir, "bakctl"), []byte("previous binary"), 0755)
			output, err := f.run(t)
			if err != nil {
				t.Fatalf("verified installation failed: %v\n%s", err, output)
			}
			installed, err := os.ReadFile(filepath.Join(f.installDir, "bakctl"))
			if err != nil || string(installed) != string(f.payload) {
				t.Fatalf("installed bytes differ: %q, %v", installed, err)
			}
			info, err := os.Stat(filepath.Join(f.installDir, "bakctl"))
			if err != nil || info.Mode().Perm() != 0755 {
				t.Fatalf("installed permissions: info=%v err=%v", info, err)
			}
			requests := f.requests(t)
			if !strings.Contains(requests, "/v9.8.7/"+platform.asset+"\n") || !strings.Contains(requests, "/v9.8.7/SHA256SUMS\n") || strings.Contains(requests, "/releases/latest") {
				t.Fatalf("wrong release requests: %s", requests)
			}
			if !strings.Contains(output, "已安装至") {
				t.Fatalf("successful install not reported: %s", output)
			}
			f.assertTemporaryFilesRemoved(t)
		})
	}
}

func TestInstallVerificationAndNetworkFailuresPreservePreviousBinary(t *testing.T) {
	for _, test := range []struct {
		name, mode, message string
		mutate              func(*testing.T, *installFixture)
	}{
		{"checksum-mismatch", "success", "校验失败", func(t *testing.T, f *installFixture) {
			writeFixture(t, filepath.Join(f.fixtures, "binary"), []byte("corrupted download"), 0600)
		}},
		{"missing-checksum", "missing_checksum", "SHA256SUMS", nil},
		{"checksum-network-error", "checksum_network_error", "SHA256SUMS", nil},
		{"binary-network-error", "binary_network_error", "二进制下载失败", nil},
		{"asset-digest-missing", "success", "校验和缺失", func(t *testing.T, f *installFixture) {
			writeFixture(t, filepath.Join(f.fixtures, "SHA256SUMS"), []byte(fmt.Sprintf("%x  different-asset\n", sha256.Sum256(f.payload))), 0600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newInstallFixture(t)
			f.env["BAKCTL_TEST_MODE"] = test.mode
			if test.mutate != nil {
				test.mutate(t, f)
			}
			previous := []byte("known working previous binary")
			writeFixture(t, filepath.Join(f.installDir, "bakctl"), previous, 0755)
			output, err := f.run(t)
			if err == nil || !strings.Contains(output, test.message) {
				t.Fatalf("failure not reported: err=%v output=%s", err, output)
			}
			got, err := os.ReadFile(filepath.Join(f.installDir, "bakctl"))
			if err != nil || string(got) != string(previous) {
				t.Fatalf("failed installation replaced previous binary: got=%q err=%v", got, err)
			}
			if strings.Contains(output, "已安装至") {
				t.Fatalf("failure reported success: %s", output)
			}
			f.assertTemporaryFilesRemoved(t)
		})
	}
}

func TestInstallLatestRelease(t *testing.T) {
	f := newInstallFixture(t)
	f.env["BAKCTL_VERSION"] = ""
	writeFixture(t, filepath.Join(f.fixtures, "latest.json"), []byte("{\n  \"tag_name\": \"v9.8.7\",\n  \"name\": \"fixture\"\n}\n"), 0600)
	output, err := f.run(t)
	if err != nil {
		t.Fatalf("latest installation failed: %v\n%s", err, output)
	}
	requests := f.requests(t)
	if !strings.HasPrefix(requests, "https://api.github.com/repos/9star-academy/bakctl/releases/latest\n") || !strings.Contains(requests, "/v9.8.7/"+f.asset) {
		t.Fatalf("latest tag not selected: %s", requests)
	}
	f.assertTemporaryFilesRemoved(t)
}

func TestLatestQueryFailureDoesNotFallbackToOlderVersion(t *testing.T) {
	for _, test := range []struct{ name, mode, response string }{
		{"network-failure", "latest_error", ""},
		{"tag-missing", "success", `{"message":"not found"}`},
		{"tag-invalid", "success", `{"tag_name":"../v1.0.1"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newInstallFixture(t)
			f.env["BAKCTL_VERSION"], f.env["BAKCTL_TEST_MODE"] = "", test.mode
			if test.response != "" {
				writeFixture(t, filepath.Join(f.fixtures, "latest.json"), []byte(test.response), 0600)
			}
			writeFixture(t, filepath.Join(f.installDir, "bakctl"), []byte("previous binary"), 0755)
			output, err := f.run(t)
			if err == nil {
				t.Fatalf("latest lookup failure ignored: %s", output)
			}
			if requests := f.requests(t); strings.Contains(requests, "/releases/download/") {
				t.Fatalf("failed latest lookup silently downloaded a release: %s", requests)
			}
			got, err := os.ReadFile(filepath.Join(f.installDir, "bakctl"))
			if err != nil || string(got) != "previous binary" {
				t.Fatalf("lookup failure changed binary: got=%q err=%v", got, err)
			}
			f.assertTemporaryFilesRemoved(t)
		})
	}
}

func TestInstallRejectsDirectoryAtBinaryPath(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink=%v", symlink), func(t *testing.T) {
			f := newInstallFixture(t)
			destination := filepath.Join(f.installDir, "bakctl")
			protected := destination
			if symlink {
				protected = filepath.Join(f.root, "unrelated directory")
			}
			if err := os.Mkdir(protected, 0700); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Symlink(protected, destination); err != nil {
					t.Fatal(err)
				}
			}
			output, err := f.run(t)
			if err == nil {
				t.Errorf("installer reported success with a directory at the binary path: %s", output)
			}
			entries, err := os.ReadDir(protected)
			if err != nil || len(entries) != 0 {
				t.Errorf("installer wrote inside the existing directory: entries=%v err=%v", entries, err)
			}
			f.assertTemporaryFilesRemoved(t)
		})
	}
}
