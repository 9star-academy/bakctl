package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func restorePayload(ctx context.Context, input, staged, legacyType string) (bool, error) {
	file, err := os.Open(input)
	if err != nil {
		return false, err
	}
	defer file.Close()
	header := make([]byte, len(payloadMagic)+5)
	n, err := io.ReadFull(file, header)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, err
	}
	if n >= len(payloadMagic) && bytes.Equal(header[:len(payloadMagic)], []byte(payloadMagic)) {
		if n != len(header) {
			return false, fmt.Errorf("备份元数据被截断")
		}
		kind := header[len(payloadMagic)]
		mode := os.FileMode(binary.BigEndian.Uint32(header[len(payloadMagic)+1:]))
		if mode & ^os.FileMode(0777) != 0 {
			return false, fmt.Errorf("备份元数据包含无效权限")
		}
		switch kind {
		case kindFile, kindRaw:
			err := writePrivateFile(staged, func(dst io.Writer) error { _, err := io.Copy(dst, &contextReader{ctx: ctx, reader: file}); return err })
			if err == nil {
				err = os.Chmod(staged, mode)
			}
			return false, err
		case kindDirectory:
			if err := extractDirectory(ctx, file, staged); err != nil {
				return true, err
			}
			return true, os.Chmod(staged, mode)
		default:
			return false, fmt.Errorf("不支持的备份内容类型: %q", kind)
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	if legacyType == "" {
		legacyType, err = inferLegacyType(file)
		if err != nil {
			return false, err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return false, err
		}
	}
	switch legacyType {
	case "raw":
		err := writePrivateFile(staged, func(dst io.Writer) error { _, err := io.Copy(dst, &contextReader{ctx: ctx, reader: file}); return err })
		return false, err
	case "file":
		return false, extractLegacyFile(ctx, file, staged)
	case "directory":
		return true, extractDirectory(ctx, file, staged)
	default:
		return false, fmt.Errorf("不支持的旧备份类型: %s", legacyType)
	}
}

func inferLegacyType(src io.Reader) (string, error) {
	tr := tar.NewReader(src)
	count := 0
	directory := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("旧备份没有类型信息，请使用 --legacy-type raw（数据库）、file 或 directory: %w", err)
		}
		count++
		if hdr.Typeflag != tar.TypeReg || strings.Contains(hdr.Name, "/") {
			directory = true
		}
	}
	if directory || count == 0 || count > 1 {
		return "directory", nil
	}
	return "", fmt.Errorf("旧备份只有一个文件，无法区分原文件和单文件目录；请指定 --legacy-type file 或 directory")
}

func extractLegacyFile(ctx context.Context, src io.Reader, staged string) error {
	tr := tar.NewReader(src)
	hdr, err := tr.Next()
	if err != nil {
		return fmt.Errorf("读取旧文件备份失败: %w", err)
	}
	if _, err := archiveName(hdr.Name); err != nil {
		return err
	}
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("旧文件备份不是单个普通文件，请指定 --legacy-type directory")
	}
	if err := writePrivateFile(staged, func(dst io.Writer) error { _, err := io.Copy(dst, &contextReader{ctx: ctx, reader: tr}); return err }); err != nil {
		return err
	}
	if _, err := tr.Next(); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("旧备份包含多个条目，请指定 --legacy-type directory")
	}
	if err := checkTarPadding(src); err != nil {
		return err
	}
	return os.Chmod(staged, os.FileMode(hdr.Mode)&0777)
}

func extractDirectory(ctx context.Context, src io.Reader, staged string) error {
	if err := os.Mkdir(staged, 0700); err != nil {
		return err
	}
	tr := tar.NewReader(src)
	seen := make(map[string]bool)
	type directoryMode struct {
		path string
		mode os.FileMode
	}
	directories := []directoryMode{}
	var symlinks []string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取目录归档失败: %w", err)
		}
		name, err := archiveName(hdr.Name)
		if err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("归档重复路径: %s", name)
		}
		seen[name] = true
		destination := filepath.Join(staged, filepath.FromSlash(name))
		if name == "." {
			if hdr.Typeflag != tar.TypeDir {
				return fmt.Errorf("归档根路径必须是目录")
			}
		} else if err := ensureArchiveParents(staged, name); err != nil {
			return err
		}
		mode := os.FileMode(hdr.Mode) & 0777
		switch hdr.Typeflag {
		case tar.TypeDir:
			if name != "." {
				if err := os.Mkdir(destination, 0700); err != nil {
					info, statErr := os.Lstat(destination)
					if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
						return fmt.Errorf("归档目录冲突: %s", name)
					}
				}
			}
			directories = append(directories, directoryMode{destination, mode})
		case tar.TypeReg:
			if name == "." {
				return fmt.Errorf("无效归档文件名")
			}
			if err := writePrivateFile(destination, func(dst io.Writer) error { _, err := io.Copy(dst, &contextReader{ctx: ctx, reader: tr}); return err }); err != nil {
				return err
			}
			if err := os.Chmod(destination, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := validateSymlink(name, hdr.Linkname); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, destination); err != nil {
				return err
			}
			symlinks = append(symlinks, name)
		default:
			return fmt.Errorf("归档包含不支持的条目类型 %d: %s", hdr.Typeflag, name)
		}
	}
	if err := checkTarPadding(src); err != nil {
		return err
	}
	for _, name := range symlinks {
		if err := validateLinkResolution(staged, name); err != nil {
			return err
		}
	}
	// Apply restrictive directory modes only after all children are written.
	for i := len(directories) - 1; i >= 0; i-- {
		if err := os.Chmod(directories[i].path, directories[i].mode); err != nil {
			return err
		}
	}
	return nil
}

func archiveName(name string) (string, error) {
	if name == "" || path.IsAbs(name) || strings.ContainsAny(name, "\\\x00") {
		return "", fmt.Errorf("不安全的归档路径: %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("归档路径越界: %q", name)
		}
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("归档路径越界: %q", name)
	}
	return clean, nil
}

func ensureArchiveParents(root, name string) error {
	parent := path.Dir(name)
	if parent == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(parent, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("拒绝通过归档符号链接或文件写入: %s", name)
		}
	}
	return nil
}

func checkTarPadding(src io.Reader) error {
	var buf [4096]byte
	for {
		n, err := src.Read(buf[:])
		for _, b := range buf[:n] {
			if b != 0 {
				return fmt.Errorf("归档结束后存在额外数据")
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
