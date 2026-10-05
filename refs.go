package ocistore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Reference 是一个可读的镜像引用：名字 + 可选标签。
// 省略标签时按惯例默认为 "latest"。
type Reference struct {
	Name string
	Tag  string
}

// String 返回 "name:tag" 形式。
func (r Reference) String() string { return r.Name + ":" + r.Tag }

// 名字与标签的合法形式（对齐 OCI distribution-spec 的常用子集）：
//   - 名字：小写字母数字开头，可含小写字母数字与 . _ - / 分隔符；
//   - 标签：字母数字或 _ 开头，后续可含字母数字与 . _ -，最长 128。
var (
	nameRe = regexp.MustCompile(`^[a-z0-9]+([._/-][a-z0-9]+)*$`)
	tagRe  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// ParseReference 解析 "name" 或 "name:tag"（省略标签时默认为 latest）。
// 名字或标签非法时分别返回 ErrInvalidName / ErrInvalidTag。
func ParseReference(s string) (Reference, error) {
	name, tag, hasTag := strings.Cut(s, ":")
	if !hasTag {
		tag = "latest"
	}
	if !nameRe.MatchString(name) {
		return Reference{}, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if !tagRe.MatchString(tag) {
		return Reference{}, fmt.Errorf("%w: %q", ErrInvalidTag, tag)
	}
	return Reference{Name: name, Tag: tag}, nil
}

// refsPath 是引用登记表的落盘位置。
func (s *Store) refsPath() string { return filepath.Join(s.root, "refs.json") }

// loadRefs 在 Open 时读取登记表；文件不存在视为空表。
func (s *Store) loadRefs() error {
	data, err := os.ReadFile(s.refsPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var m map[string]Digest
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("ocistore: corrupt refs file: %w", err)
	}
	for k, d := range m {
		if _, err := ParseReference(k); err != nil {
			return fmt.Errorf("ocistore: corrupt refs file: bad key %q: %w", k, err)
		}
		if err := d.Validate(); err != nil {
			return fmt.Errorf("ocistore: corrupt refs file: bad digest for %q: %w", k, err)
		}
		s.refs[k] = d
	}
	return nil
}

// saveRefsLocked 把当前登记表原子落盘（调用方须持有 refsMu）。
// 先写临时文件再 rename，任何时刻读者看到的都是完整结果。
func (s *Store) saveRefsLocked() error {
	data, err := json.MarshalIndent(s.refs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "refs-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.refsPath())
}

// Tag 把引用 ref 登记（或改指向）到摘要 d。
// 校验失败原因可区分：引用非法 ErrInvalidName/ErrInvalidTag、
// 摘要格式非法 ErrInvalidDigest、摘要内容不存在 ErrNotFound。
// 改指向后，以最新登记为准；并发登记/改指向通过互斥锁 + 原子落盘
// 保证不留半截结果、不丢完整记录。
func (s *Store) Tag(ref string, d Digest) error {
	r, err := ParseReference(ref)
	if err != nil {
		return err
	}
	if err := d.Validate(); err != nil {
		return err
	}
	if !s.Has(d) {
		return fmt.Errorf("%w: cannot tag %q to missing blob %s", ErrNotFound, r, d)
	}
	s.refsMu.Lock()
	defer s.refsMu.Unlock()
	s.refs[r.String()] = d
	return s.saveRefsLocked()
}

// ResolveRef 按引用查回登记的摘要；未登记返回 ErrRefNotFound。
func (s *Store) ResolveRef(ref string) (Digest, error) {
	r, err := ParseReference(ref)
	if err != nil {
		return "", err
	}
	s.refsMu.Lock()
	defer s.refsMu.Unlock()
	d, ok := s.refs[r.String()]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrRefNotFound, r)
	}
	return d, nil
}

// Untag 删除一个已登记的引用；未登记返回 ErrRefNotFound。
func (s *Store) Untag(ref string) error {
	r, err := ParseReference(ref)
	if err != nil {
		return err
	}
	s.refsMu.Lock()
	defer s.refsMu.Unlock()
	if _, ok := s.refs[r.String()]; !ok {
		return fmt.Errorf("%w: %q", ErrRefNotFound, r)
	}
	delete(s.refs, r.String())
	return s.saveRefsLocked()
}

// ListRefs 列出当前全部登记（"name:tag" -> 摘要），键按字典序排序后
// 以稳定顺序返回（map 本身无序，调用方需要顺序时用 RefNames）。
func (s *Store) ListRefs() (map[string]Digest, error) {
	s.refsMu.Lock()
	defer s.refsMu.Unlock()
	out := make(map[string]Digest, len(s.refs))
	for k, d := range s.refs {
		out[k] = d
	}
	return out, nil
}

// RefNames 按字典序返回全部已登记引用名。
func (s *Store) RefNames() ([]string, error) {
	m, err := s.ListRefs()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}
