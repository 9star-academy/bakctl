package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

const payloadMagic = "\x00BAKCTL2\n"
const (
	kindFile      byte = 'f'
	kindDirectory byte = 'd'
	kindRaw       byte = 'r'
)

func isPostgres(src string) bool {
	return strings.HasPrefix(src, "pg://") || strings.HasPrefix(src, "postgres://") || strings.HasPrefix(src, "postgresql://")
}

func prepareSource(ctx context.Context, src, workspace string) (string, byte, os.FileMode, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, 0, err
	}
	if strings.HasPrefix(src, "sqlite://") {
		dbPath, err := filepath.Abs(strings.TrimPrefix(src, "sqlite://"))
		if err != nil {
			return "", 0, 0, err
		}
		info, err := os.Stat(dbPath)
		if err != nil {
			return "", 0, 0, fmt.Errorf("SQLite 数据库不存在: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", 0, 0, fmt.Errorf("SQLite 源不是普通文件")
		}
		sqlite, err := exec.LookPath("sqlite3")
		if err != nil {
			return "", 0, 0, fmt.Errorf("SQLite 在线备份需要 sqlite3；拒绝直接复制运行中的数据库: %w", err)
		}
		// The private working directory permits a fixed, safely quoted output
		// name regardless of special characters in the system temporary path.
		cmd := exec.CommandContext(ctx, sqlite, "-batch", "-bail", "-readonly", dbPath, ".timeout 5000", ".backup snapshot.db")
		cmd.Dir = workspace
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", 0, 0, fmt.Errorf("SQLite 在线备份失败: %s (%w)", strings.TrimSpace(string(output)), err)
		}
		return filepath.Join(workspace, "snapshot.db"), kindRaw, 0600, nil
	}
	if isPostgres(src) {
		uri := src
		if strings.HasPrefix(uri, "pg://") {
			uri = "postgres://" + strings.TrimPrefix(uri, "pg://")
		}
		output := filepath.Join(workspace, "database.sql")
		err := writePrivateFile(output, func(dst io.Writer) error {
			cmd := exec.CommandContext(ctx, "pg_dump", uri)
			cmd.Stdout = dst
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("pg_dump 执行失败: %s (%w)", strings.TrimSpace(stderr.String()), err)
			}
			return nil
		})
		if err != nil {
			return "", 0, 0, err
		}
		return output, kindRaw, 0600, nil
	}
	if strings.Contains(src, "://") {
		return "", 0, 0, fmt.Errorf("不支持的备份源协议")
	}
	absolute, err := filepath.Abs(src)
	if err != nil {
		return "", 0, 0, err
	}
	source, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", 0, 0, err
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", 0, 0, err
	}
	if info.IsDir() {
		return source, kindDirectory, info.Mode().Perm(), nil
	}
	if info.Mode().IsRegular() {
		return source, kindFile, info.Mode().Perm(), nil
	}
	return "", 0, 0, fmt.Errorf("备份源必须是普通文件或目录")
}

func writeSource(ctx context.Context, dst io.Writer, source string, kind byte, mode os.FileMode) (int64, error) {
	header := make([]byte, len(payloadMagic)+5)
	copy(header, payloadMagic)
	header[len(payloadMagic)] = kind
	binary.BigEndian.PutUint32(header[len(payloadMagic)+1:], uint32(mode.Perm()))
	if _, err := dst.Write(header); err != nil {
		return 0, err
	}
	if kind != kindDirectory {
		return copyStableFile(ctx, dst, source)
	}
	tw := tar.NewWriter(dst)
	var size int64
	err := filepath.Walk(source, func(current string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if _, err := archiveName(name); err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(current)
			if err != nil {
				return err
			}
			if err := validateSymlink(name, link); err != nil {
				return err
			}
			if err := validateLinkResolution(source, name); err != nil {
				return err
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("不支持备份特殊文件: %s", current)
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			n, err := copyStableFile(ctx, tw, current)
			size += n
			return err
		}
		return nil
	})
	closeErr := tw.Close()
	if err != nil {
		return size, err
	}
	return size, closeErr
}

func copyStableFile(ctx context.Context, dst io.Writer, source string) (int64, error) {
	pathInfo, err := os.Lstat(source)
	if err != nil {
		return 0, err
	}
	if !pathInfo.Mode().IsRegular() {
		return 0, fmt.Errorf("源文件在备份期间被替换: %s", source)
	}
	file, err := os.Open(source)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if !before.Mode().IsRegular() || !os.SameFile(pathInfo, before) {
		return 0, fmt.Errorf("源在备份期间被替换: %s", source)
	}
	n, err := io.CopyN(dst, &contextReader{ctx: ctx, reader: file}, before.Size())
	if err != nil {
		return n, err
	}
	after, err := file.Stat()
	if err != nil {
		return n, err
	}
	finalPathInfo, err := os.Lstat(source)
	if err != nil {
		return n, err
	}
	if !finalPathInfo.Mode().IsRegular() || !os.SameFile(before, finalPathInfo) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return n, fmt.Errorf("文件在备份期间发生改变，请重试: %s", source)
	}
	return n, nil
}

func validateSymlink(name, target string) error {
	if target == "" || strings.ContainsRune(target, '\x00') || strings.Contains(target, "\\") || path.IsAbs(target) {
		return fmt.Errorf("符号链接目标必须位于备份目录内: %s -> %s", name, target)
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("符号链接逃逸备份目录: %s -> %s", name, target)
	}
	return nil
}

// validateLinkResolution expands the real symlink graph one path component at a
// time. Lexical Clean alone is insufficient for e.g. sub/link/../outside when
// sub/link itself points to '..'. Missing in-root targets remain valid dangling
// links, but no expansion may step outside the archive root.
func validateLinkResolution(root, name string) error {
	pending := strings.Split(name, "/")
	var resolved []string
	expansions := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return fmt.Errorf("符号链接链逃逸备份目录: %s", name)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		parts := append(append([]string{root}, resolved...), part)
		current := filepath.Join(parts...)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			resolved = append(resolved, part)
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = append(resolved, part)
			continue
		}
		expansions++
		if expansions > 255 {
			return fmt.Errorf("符号链接循环或链过长: %s", name)
		}
		target, err := os.Readlink(current)
		if err != nil {
			return err
		}
		if path.IsAbs(target) || strings.Contains(target, "\\") {
			return fmt.Errorf("符号链接链包含外部目标: %s", name)
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	return nil
}

// resolvedPath also resolves an existing symlink ancestor of a destination that
// has not been created yet, so an apparent outside target cannot point back in.
func resolvedPath(name string) (string, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	var missing []string
	current := absolute
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
