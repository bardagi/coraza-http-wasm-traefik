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

	if err = sh.RunV("go", "run", "github.com/corazawaf/coraza/v3/http/e2e/cmd/httpe2e@main", "--proxy-hostport",
		"http://"+proxyHost, "--httpbin-hostport", "http://"+httpbinHost); err != nil {
		_ = sh.RunV("docker", append(dockerComposeArgs, "logs", service)...)
	}

	return err
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
