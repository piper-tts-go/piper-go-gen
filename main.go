package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/mholt/archiver/v4"
	"github.com/rs/zerolog/log"
	"github.com/zeebo/xxh3"
)

type Meta struct {
	Version string
	Hash    xxh3.Uint128
}

const (
	ArchiveFilename  = "dist.tzst"
	MetadataFilename = "dist.json"
)

func download(rootDir string, srcURL string) (filename string, retErr error) {
	log.Info().Str("url", srcURL).Msg("downloading file")
	filename = filepath.Join(
		rootDir,
		"piper-gen.cache",
		url.QueryEscape(srcURL),
	)
	if _, err := os.Stat(filename); err == nil {
		return filename, nil
	}

	os.MkdirAll(filepath.Dir(filename), 0o755)

	out, err := os.Create(filename)
	defer func() {
		closeErr := out.Close()
		if closeErr != nil && retErr == nil {
			retErr = closeErr
		}
		if retErr != nil {
			retErr = fmt.Errorf("failed to download %q: %w", srcURL, retErr)
		}
	}()

	response, err := http.Get(srcURL)
	if err != nil {
		return "", fmt.Errorf("failed to download %q: %w", srcURL, err)
	}
	defer response.Body.Close()

	if _, err := io.Copy(out, response.Body); err != nil {
		return "", fmt.Errorf("failed to download %q: %w", srcURL, err)
	}
	return filename, nil
}

func Extract(ctx context.Context, rootDir string, f archiver.File) (retErr error) {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failed to read file info: %w", err)
	}
	if info.IsDir() {
		return nil
	}

	filename := filepath.Join(rootDir, filepath.Clean(filepath.FromSlash(f.NameInArchive)))
	if _, err := os.Stat(filename); err == nil {
		return nil
	}

	os.MkdirAll(filepath.Dir(filename), 0o755)

	if info.Mode().Type()&os.ModeSymlink == os.ModeSymlink {
		err := os.Symlink(f.LinkTarget, filename)
		if err != nil {
			return fmt.Errorf("failed to symlink %q to %q: %w", filename, f.LinkTarget, err)
		}
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	reader, err := f.Open()
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer reader.Close()
	writer, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, f.Mode().Perm())
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() {
		closeErr := writer.Close()
		if retErr != nil {
			return
		}
		if closeErr != nil {
			retErr = fmt.Errorf("failed to close file: %w", closeErr)
		}
	}()
	if _, err := io.Copy(writer, reader); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}
	return nil
}

type voiceInfo struct {
	ONNX      string
	ModelCard string
	JSON      string
}

func hashFile(h hash.Hash, filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	return nil
}

func run(workingDirectory string, program string, args ...string) error {
	stderr := bytes.NewBuffer(nil)
	cmd := exec.Command(program, args...)
	cmd.Stderr = stderr
	cmd.Stdout = stderr
	cmd.Dir = workingDirectory
	log.Info().Str("program", program).Strs("args", args).Msg("running executable command")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run `%s %s`: %w: %s", program, strings.Join(args, " "), err, stderr.Bytes())
	}
	return nil
}

func generatePackage(voicePkg bool, pkgDir, embedPkgName, pkgPath string, assetName string, version string, embedPaths ...string) error {
	embedPaths = append([]string{
		ArchiveFilename,
		MetadataFilename,
	}, embedPaths...)

	embedGo := []byte(`// GENERATED FILE

package ` + embedPkgName + `

import (
	"embed"
	"github.com/piper-tts-go/piper-go-asset"
)

var (
	//go:embed ` + strings.Join(embedPaths, " ") + `
	fs embed.FS

	Asset = asset.Asset{Name: "` + assetName + `", FS: fs}
)
`)
	goMod := []byte(`
module ` + pkgPath + `

go 1.21

`)

	license := []byte(`
MIT License

Copyright (c) 2023 Amity Bell
Copyright (c) 2025 Dharma Bellamkonda

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
`)

	distLicense := "https://github.com/piper-tts-go/piper"
	if voicePkg {
		distLicense = "[MODEL_CARD.txt](MODEL_CARD.txt)"
	}

	readmeMd := []byte(`
Package auto-generated by https://github.com/piper-tts-go/piper-gen

- Package license: See [LICENSE](LICENSE)
- dist.tar.zst license: See ` + distLicense + `
- See https://github.com/piper-tts-go/piper for docs
`)

	if err := os.WriteFile(filepath.Join(pkgDir, "embed.go"), embedGo, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "go.mod"), goMod, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "README.md"), readmeMd, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "LICENSE"), license, 0o644); err != nil {
		return err
	}
	if err := installMeta(pkgDir, version, filepath.Join(pkgDir, ArchiveFilename)); err != nil {
		return err
	}
	if err := run(pkgDir, "go", "mod", "tidy"); err != nil {
		return err
	}
	if err := run(pkgDir, "go", "build", "."); err != nil {
		return err
	}
	return nil
}

func installMeta(dir string, version string, filenames ...string) error {
	filenames = append([]string(nil), filenames...)
	sort.Strings(filenames)

	h := xxh3.New()
	for _, filename := range filenames {
		if err := hashFile(h, filename); err != nil {
			return fmt.Errorf("failed to hash file %q: %w", filename, err)
		}
	}
	src, err := json.Marshal(Meta{
		Version: version,
		Hash:    h.Sum128(),
	})
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, MetadataFilename), src, 0o644); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}
	return nil
}

func copyFile(dest, src string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open %q: %w", src, err)
	}
	defer srcFile.Close()

	destFile, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("failed to create %q: %w", dest, err)
	}
	_, copyErr := io.Copy(destFile, srcFile)
	closeErr := destFile.Close()
	if copyErr != nil {
		return fmt.Errorf("failed to copy %q to %q: %w", src, dest, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("failed to close %q: %w", dest, closeErr)
	}
	return nil
}

func installVoice(rootDir, name string, version string, urls []string) error {
	packageName := "piper-voice-" + name
	packageDirectory := filepath.Join(rootDir, packageName)
	packagePath := "github.com/piper-tts-go/" + packageName

	archiveFilename := filepath.Join(packageDirectory, ArchiveFilename)
	tarball, err := newTarball(archiveFilename)
	if err != nil {
		return fmt.Errorf("failed to create tarball: %w", err)
	}

	modelFilename := ""
	for _, url := range urls {
		basename := filepath.Base(url)
		extension := filepath.Ext(basename)
		switch {
		case basename == "MODEL_CARD":
		case extension == ".onnx":
			basename = "voice.onnx"
		case extension == ".json":
			basename = "voice.json"
		default:
			return fmt.Errorf("encountered unexpected file extension %q", extension)
		}
		filename, err := download(rootDir, url)
		if err != nil {
			return fmt.Errorf("failed to download voice: %w", err)
		}
		if err := tarball.AppendFile(basename, filename); err != nil {
			return fmt.Errorf("failed to add %q to tarball: %w", filename, err)
		}
		if basename == "MODEL_CARD" {
			modelFilename = filename
		}
	}

	if err := tarball.Close(); err != nil {
		return fmt.Errorf("failed to close tarball: %w", err)
	}
	if err := copyFile(filepath.Join(packageDirectory, "MODEL_CARD.txt"), modelFilename); err != nil {
		return fmt.Errorf("failed to copy MODEL_CARD.txt into package: %w", err)
	}
	if err := generatePackage(true, packageDirectory, name, packagePath, name, version, "MODEL_CARD.txt"); err != nil {
		return fmt.Errorf("failed to generate package: %w", err)
	}
	return nil
}

func installPiper(ctx context.Context, rootDir, pkgName, version, url string) (retErr error) {
	packageName := "piper-bin-" + pkgName
	packageDirectory := filepath.Join(rootDir, packageName)
	packagePath := "github.com/piper-tts-go/" + packageName
	filename, err := download(rootDir, url)
	if err != nil {
		return fmt.Errorf("failed to download piper: %w", err)
	}
	srcFile, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to open %q: %w", filename, err)
	}
	defer srcFile.Close()

	format, stream, err := archiver.Identify(srcFile.Name(), srcFile)
	if err != nil {
		return fmt.Errorf("could not identify %q: %w", srcFile.Name(), err)
	}

	extractor, ok := format.(archiver.Extractor)
	if !ok {
		return fmt.Errorf("%T is not an archiver.Extractor: `%s`", format, srcFile.Name())
	}

	destFilename := filepath.Join(packageDirectory, ArchiveFilename)
	tarball, err := newTarball(destFilename)
	if err != nil {
		return fmt.Errorf("failed to create tarball: %w", err)
	}
	err = extractor.Extract(
		ctx,
		stream,
		[]string{"piper"},
		func(ctx context.Context, f archiver.File) error {
			fileMode := f.Mode()
			if !fileMode.IsRegular() && fileMode&os.ModeSymlink == 0 {
				return nil
			}
			reader, err := f.Open()
			if err != nil {
				return err
			}
			defer reader.Close()
			header := &tar.Header{
				Name:     strings.TrimPrefix(f.NameInArchive, "piper/"),
				Mode:     int64(f.Mode()),
				Size:     f.Size(),
				Linkname: f.LinkTarget,
			}
			if fileMode&os.ModeSymlink != 0 {
				header.Typeflag = tar.TypeSymlink
			}
			return tarball.Append(header, reader)
		},
	)
	if e := tarball.Close(); e != nil && err == nil {
		return fmt.Errorf("failed to close tarball: %w", e)
	}
	if err != nil {
		return fmt.Errorf("failed to extract piper: %w", err)
	}
	if err := generatePackage(false, packageDirectory, pkgName, packagePath, pkgName, version); err != nil {
		return fmt.Errorf("failed to generate package: %w", err)
	}
	return nil
}

func main() {
	ctx := context.Background()
	dir := flag.String("dir", "", "root directory to extract store files")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "-dir is required.")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// more voices at https://huggingface.co/rhasspy/piper-voices/tree/v1.0.0
	voiceVersion := "1.0.0"
	urlPrefix := "https://huggingface.co/rhasspy/piper-voices/resolve/v" + voiceVersion
	voices := map[string][]string{
		"jenny": {
			urlPrefix + "/en/en_GB/jenny_dioco/medium/en_GB-jenny_dioco-medium.onnx",
			urlPrefix + "/en/en_GB/jenny_dioco/medium/en_GB-jenny_dioco-medium.onnx.json",
			urlPrefix + "/en/en_GB/jenny_dioco/medium/MODEL_CARD",
		},
		"alan": {
			urlPrefix + "/en/en_GB/alan/medium/en_GB-alan-medium.onnx",
			urlPrefix + "/en/en_GB/alan/medium/MODEL_CARD",
			urlPrefix + "/en/en_GB/alan/medium/en_GB-alan-medium.onnx.json",
		},
		"kristin": {
			urlPrefix + "/en/en_US/kristin/medium/en_US-kristin-medium.onnx",
			urlPrefix + "/en/en_US/kristin/medium/MODEL_CARD",
			urlPrefix + "/en/en_US/kristin/medium/en_US-kristin-medium.onnx.json",
		},
		"bryce": {
			urlPrefix + "/en/en_US/bryce/medium/en_US-bryce-medium.onnx",
			urlPrefix + "/en/en_US/bryce/medium/MODEL_CARD",
			urlPrefix + "/en/en_US/bryce/medium/en_US-bryce-medium.onnx.json",
		},
	}
	for name, urls := range voices {
		if err := installVoice(*dir, name, voiceVersion, urls); err != nil {
			log.Fatal().Err(err).Str("voice", name).Msg("failed to install voice")
		}
	}

	piperVersion := "v2.0.0"
	archives := map[string]string{
		"linux":   "https://github.com/piper-tts-go/piper/releases/download/" + piperVersion + "/piper_linux_x86_64.tar.gz",
		"windows": "https://github.com/piper-tts-go/piper/releases/download/" + piperVersion + "/piper_windows_amd64.zip",
		"darwin":  "https://github.com/piper-tts-go/piper/releases/download/" + piperVersion + "/piper_macos_aarch64.tar.gz",
	}
	for plaform, url := range archives {
		if err := installPiper(ctx, *dir, plaform, piperVersion, url); err != nil {
			log.Fatal().Err(err).Str("platform", plaform).Msg("failed to install piper")
		}
	}
}

type Tarball struct {
	file    *os.File
	encoder *zstd.Encoder
	writer  *tar.Writer
}

func newTarball(filename string, opts ...zstd.EOption) (*Tarball, error) {
	if opts == nil {
		opts = []zstd.EOption{
			zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		}
	}

	os.MkdirAll(filepath.Dir(filename), 0755)
	file, err := os.Create(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to create file %q: %w", filename, err)
	}

	encoder, err := zstd.NewWriter(file, opts...)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to create zstd encoder: %w", err)
	}

	writer := &Tarball{
		file:    file,
		encoder: encoder,
		writer:  tar.NewWriter(encoder),
	}
	return writer, nil
}

func (tb *Tarball) Append(h *tar.Header, r io.Reader) error {
	if err := tb.writer.WriteHeader(h); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}
	if _, err := io.Copy(tb.writer, r); err != nil {
		return fmt.Errorf("failed to copy data: %w", err)
	}
	if err := tb.writer.Flush(); err != nil {
		return fmt.Errorf("failed to flush data: %w", err)
	}
	return nil
}

func (tb *Tarball) AppendFile(dest, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open %q: %w", src, err)
	}
	defer f.Close()

	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("failed to read file info: %w", err)
	}
	header := &tar.Header{
		Name: dest,
		Mode: int64(info.Mode()),
		Size: info.Size(),
	}
	if info.Mode()&os.ModeSymlink != 0 {
		nm, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("failed to read symlink: %w", err)
		}
		header.Linkname = nm
	}
	if err := tb.Append(header, f); err != nil {
		return fmt.Errorf("failed to append file %q: %w", src, err)
	}
	return nil
}

func (tb *Tarball) Close() (err error) {
	if closeErr := tb.writer.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("failed to close writer: %w", closeErr))
	}
	if closeErr := tb.encoder.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("failed to close encoder: %w", closeErr))
	}
	if closeErr := tb.file.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("failed to close file: %w", closeErr))
	}
	return
}
