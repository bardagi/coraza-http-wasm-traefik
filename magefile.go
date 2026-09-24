//go:build mage

package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "embed"

	"github.com/magefile/mage/sh"
)

const (
	// httpWasmRepo is the GitHub repository publishing the coraza-http-wasm binary.
	httpWasmRepo = "bardagi/coraza-http-wasm"
	artifactURL  = "https://github.com/" + httpWasmRepo + "/releases/download/{version}/coraza-http-wasm-{version}.zip"
	buildDir     = "build"
)

var httpClient = &http.Client{Timeout: 5 * time.Minute}

func download(url, dst string) error {
	fmt.Printf("Downloading %s to %s\n", url, dst)

	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Check the status before creating the file so a failed download does not
	// leave an empty or partial file behind.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: bad status: %s", url, resp.Status)
	}

	return writeFile(dst, resp.Body)
}

// writeFile writes r to dst, removing dst if anything fails.
func writeFile(dst string, r io.Reader) (err error) {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()

	_, err = io.Copy(out, r)
	return err
}

func unzip(name string) error {
	fmt.Printf("Unzipping %s\n", name)
	reader, err := zip.OpenReader(name)
	if err != nil {
		return err
	}
	defer reader.Close()

	dir := filepath.Dir(name)
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		if err := extractFile(file, dir); err != nil {
			return err
		}
	}

	return nil
}

func extractFile(file *zip.File, dir string) error {
	// Guard against zip-slip: entries must not escape the target directory.
	dst := filepath.Join(dir, file.Name)
	if !strings.HasPrefix(dst, filepath.Clean(dir)+string(os.PathSeparator)) {
		return fmt.Errorf("illegal file path in archive: %s", file.Name)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	in, err := file.Open()
	if err != nil {
		return err
	}
	defer in.Close()

	return writeFile(dst, in)
}

func downloadHTTPWasmArtifact(version, dir string) error {
	url := strings.ReplaceAll(artifactURL, "{version}", version)
	zipPath := filepath.Join(dir, "coraza-http-wasm.zip")
	if err := download(url, zipPath); err != nil {
		return err
	}
	defer os.Remove(zipPath)

	return unzip(zipPath)
}

func getHttpWasmVersion() (string, error) {
	if version := os.Getenv("VERSION"); version != "" {
		return version, nil
	}

	// releases/latest skips drafts and pre-releases, unlike the first entry of
	// the releases list.
	version, err := sh.Output("gh", "api", "repos/"+httpWasmRepo+"/releases/latest", "-q", ".tag_name")
	if err != nil {
		return "", err
	}
	if version == "" {
		return "", fmt.Errorf("no release found for %s", httpWasmRepo)
	}
	return version, nil
}

func DownloadArtifact() error {
	if err := os.MkdirAll(buildDir, 0755); err != nil {
		return err
	}

	version, err := getHttpWasmVersion()
	if err != nil {
		return err
	}

	return downloadHTTPWasmArtifact(version, buildDir)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	return writeFile(dst, in)
}

type e2eSource string

const (
	e2eSourceLocal  e2eSource = "local"
	e2eSourceRemote e2eSource = "remote"
)

func e2e(source e2eSource) error {
	var err error

	dockerComposeArgs := []string{"compose", "-f", "docker-compose.yml", "-f", "e2e/docker-compose.e2e.yml"}
	service := "e2e_traefik_" + string(source)

	if err = sh.RunV("docker", append(dockerComposeArgs, "up", "-d", service)...); err != nil {
		return err
	}
	defer func() {
		_ = sh.RunV("docker", append(dockerComposeArgs, "down", "-v")...)
	}()

	proxyHost := os.Getenv("TRAEFIK_HOST")
	if proxyHost == "" {
		proxyHost = "localhost:8080"
	}
	httpbinHost := os.Getenv("HTTPBIN_HOST")
	if httpbinHost == "" {
		httpbinHost = "localhost:8000"
	}

	if err = runE2ETests("http://"+proxyHost, "http://"+httpbinHost); err != nil {
		_ = sh.RunV("docker", append(dockerComposeArgs, "logs", service)...)
	}

	return err
}

// blockedBody is the response body coraza-http-wasm returns on interruptions.
const blockedBody = "Request blocked\n"

type e2eTestCase struct {
	name, method, path, body, userAgent string
	status                              int
	missingConfigHeader                 bool
}

// e2eTestCases mirror the scenarios of corazawaf/coraza's httpe2e suite, whose
// runner requires empty denial bodies whereas coraza-http-wasm returns a
// generic message. They match the rules in e2e/config-dynamic.yaml.
var e2eTestCases = []e2eTestCase{
	{name: "configuration", method: "GET", path: "/", status: 424, missingConfigHeader: true},
	{name: "allowed", method: "GET", path: "/?arg=arg_1", status: 200},
	{name: "URI deny", method: "GET", path: "/admin", status: 403},
	{name: "allowed body", method: "POST", path: "/anything", body: "This is a legit payload", status: 200},
	{name: "request body deny", method: "POST", path: "/anything", body: "maliciouspayload", status: 403},
	{name: "response header deny", method: "GET", path: "/response-headers?pass=leak", status: 403},
	{name: "response body deny", method: "POST", path: "/anything", body: "responsebodycode", status: 403},
	{name: "XSS", method: "GET", path: "/anything?arg=%3Cscript%3Ealert(0)%3C/script%3E", status: 403},
	{name: "SQLi", method: "POST", path: "/anything", body: "1%27%20ORDER%20BY%203--%2B", status: 403},
	{name: "scanner", method: "GET", path: "/anything", userAgent: "Grabber/0.1 (X11; U; Linux i686; en-US; rv:1.7)", status: 403},
}

func runE2ETests(proxyURL, httpbinURL string) error {
	client := &http.Client{Timeout: 10 * time.Second}

	// Traefik may need a while to fetch and compile the plugin, so wait until
	// both the upstream and the proxy (with the WAF active) respond.
	if err := waitForStatus(client, httpbinURL+"/status/200", nil, http.StatusOK); err != nil {
		return err
	}
	if err := waitForStatus(client, proxyURL+"/", http.Header{"Coraza-E2e": {"ok"}}, http.StatusOK); err != nil {
		return err
	}

	var failed int
	for i, tc := range e2eTestCases {
		fmt.Printf("[%d/%d] %s: ", i+1, len(e2eTestCases), tc.name)
		if err := runE2ETestCase(client, proxyURL, tc); err != nil {
			failed++
			fmt.Printf("FAIL: %v\n", err)
			continue
		}
		fmt.Println("ok")
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d e2e tests failed", failed, len(e2eTestCases))
	}
	return nil
}

func runE2ETestCase(client *http.Client, proxyURL string, tc e2eTestCase) error {
	req, err := http.NewRequest(tc.method, proxyURL+tc.path, strings.NewReader(tc.body))
	if err != nil {
		return err
	}
	if !tc.missingConfigHeader {
		req.Header.Set("coraza-e2e", "ok")
	}
	if tc.body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if tc.userAgent != "" {
		req.Header.Set("User-Agent", tc.userAgent)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != tc.status {
		return fmt.Errorf("expected status %d, got %d", tc.status, resp.StatusCode)
	}
	if tc.status == http.StatusOK {
		return nil
	}

	if string(body) != blockedBody {
		return fmt.Errorf("expected body %q, got %q", blockedBody, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		return fmt.Errorf("unexpected Content-Type %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		return fmt.Errorf("unexpected Cache-Control %q", cc)
	}
	return nil
}

func waitForStatus(client *http.Client, url string, header http.Header, status int) error {
	const timeout = 60 * time.Second
	fmt.Printf("Waiting for %s to return %d\n", url, status)

	var lastErr error
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(time.Second) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header = header.Clone()

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == status {
			return nil
		}
		lastErr = fmt.Errorf("got status %d", resp.StatusCode)
	}

	return fmt.Errorf("timeout waiting for %s: %v", url, lastErr)
}

func E2E() error {
	return e2e(e2eSourceRemote)
}

func E2ELocal() error {
	// DownloadArtifact creates the build directory, so it must run before
	// copying the manifest into it.
	if err := DownloadArtifact(); err != nil {
		return err
	}

	if err := copyFile(".traefik.yml", filepath.Join(buildDir, ".traefik.yml")); err != nil {
		return err
	}

	return e2e(e2eSourceLocal)
}

//go:embed config-static.yaml.tmpl
var staticConfig string

func UpdateVersion() error {
	version := os.Getenv("VERSION")
	if version == "" {
		return errors.New("VERSION environment variable is not set")
	}

	renderedStaticConfig := strings.ReplaceAll(staticConfig, "{{version}}", version)

	return errors.Join(
		os.WriteFile("config-static.yaml", []byte(renderedStaticConfig), 0644),
		os.WriteFile("e2e/config-static.remote.yaml", []byte(renderedStaticConfig), 0644),
	)
}
