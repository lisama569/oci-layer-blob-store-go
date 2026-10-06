package ocistore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// DefaultTag 是未显式指定标签时使用的默认标签。
const DefaultTag = "latest"

// TagRecord 描述一条已登记的名字/标签指向。
type TagRecord struct {
	Name   string `json:"name"`
	Tag    string `json:"tag"`
	Digest Digest `json:"digest"`
}

// 磁盘上的持久化结构：<root>/refs/tags.json。
type persistedRefs struct {
	// Records 是扁平的登记记录列表，原子整体重写。
	Records []TagRecord `json:"records"`
}

var (
	// 名字：由 '/' 分隔的若干段，每段为小写字母数字，内部可含 '.'、'_'、'-'。
	nameSegmentRe = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	// 标签：大写/小写字母、数字、'.'、'_'、'-'，1~128 字符。
	tagRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

func (s *Store) refsPath() string { return filepath.Join(s.root, "refs", "tags.json") }
func (s *Store) lockPath() string { return filepath.Join(s.root, "refs", ".lock") }

func validateName(name string) error {
	if name == "" || len(name) > 255 {
		return fmt.Errorf("%w: %q (empty or too long)", ErrInvalidName, name)
	}
	for _, seg := range splitSegments(name) {
		if seg == "" || !nameSegmentRe.MatchString(seg) {
			return fmt.Errorf("%w: %q (bad segment %q)", ErrInvalidName, name, seg)
		}
	}
	return nil
}

func splitSegments(name string) []string {
	var segs []string
	start := 0
	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			segs = append(segs, name[start:i])
			start = i + 1
		}
	}
	return append(segs, name[start:])
}

func validateTag(tag string) error {
	if !tagRe.MatchString(tag) {
		return fmt.Errorf("%w: %q", ErrInvalidTag, tag)
	}
	return nil
}

// normalizeTag 空标签归一为 DefaultTag。
func normalizeTag(tag string) string {
	if tag == "" {
		return DefaultTag
	}
	return tag
}

// loadRefsLocked 从磁盘读取持久化登记。文件不存在时视为空。
// 调用方必须持有对应的跨进程锁。
func (s *Store) loadRefsLocked() (map[string]map[string]Digest, error) {
	data, err := os.ReadFile(s.refsPath())
	if os.IsNotExist(err) {
		return map[string]map[string]Digest{}, nil
	}
	if err != nil {
		return nil, err
	}
	var p persistedRefs
	if len(data) > 0 {
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("ocistore: corrupt refs file: %w", err)
		}
	}
	out := map[string]map[string]Digest{}
	for _, r := range p.Records {
		if out[r.Name] == nil {
			out[r.Name] = map[string]Digest{}
		}
		out[r.Name][r.Tag] = r.Digest
	}
	return out, nil
}

// saveRefsLocked 将登记映射整体原子写回磁盘（tmp + rename）。
// 调用方必须持有排他的跨进程锁。
func (s *Store) saveRefsLocked(refs map[string]map[string]Digest) error {
	p := persistedRefs{}
	for name, tags := range refs {
		for tag, d := range tags {
			p.Records = append(p.Records, TagRecord{Name: name, Tag: tag, Digest: d})
		}
	}
	sort.Slice(p.Records, func(i, j int) bool {
		if p.Records[i].Name != p.Records[j].Name {
			return p.Records[i].Name < p.Records[j].Name
		}
		return p.Records[i].Tag < p.Records[j].Tag
	})
	data, err := json.MarshalIndent(&p, "", "  ")
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

// Tag 把 name:tag 登记（或改指向）到摘要 d。
//
// tag 为空时使用 "latest"。名字/标签非法返回 ErrInvalidName/ErrInvalidTag；
// 摘要格式非法返回 ErrInvalidDigest；摘要对应内容本地不存在返回 ErrNotFound。
// 同一名字的并发登记/改指向通过跨进程文件锁串行化，落盘为原子整体重写，
// 不会出现半写记录或互相覆盖丢数据。
func (s *Store) Tag(name, tag string, d Digest) error {
	if err := validateName(name); err != nil {
		return err
	}
	tag = normalizeTag(tag)
	if err := validateTag(tag); err != nil {
		return err
	}
	if err := d.Validate(); err != nil {
		return err
	}
	if !s.Has(d) {
		return fmt.Errorf("%w: cannot tag %s:%s to missing digest %s", ErrNotFound, name, tag, d)
	}

	lk, err := acquireFileLock(s.lockPath(), true)
	if err != nil {
		return err
	}
	defer lk.release()

	refs, err := s.loadRefsLocked()
	if err != nil {
		return err
	}
	if refs[name] == nil {
		refs[name] = map[string]Digest{}
	}
	refs[name][tag] = d
	return s.saveRefsLocked(refs)
}

// Lookup 按名字与标签查回登记的摘要；未登记返回 ErrNameNotFound。
func (s *Store) Lookup(name, tag string) (Digest, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	tag = normalizeTag(tag)
	if err := validateTag(tag); err != nil {
		return "", err
	}

	lk, err := acquireFileLock(s.lockPath(), false)
	if err != nil {
		return "", err
	}
	defer lk.release()

	refs, err := s.loadRefsLocked()
	if err != nil {
		return "", err
	}
	tags, ok := refs[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNameNotFound, name)
	}
	d, ok := tags[tag]
	if !ok {
		return "", fmt.Errorf("%w: %s:%s", ErrNameNotFound, name, tag)
	}
	return d, nil
}

// Untag 删除一条名字/标签登记。名字下最后一个标签被删后，名字随之消失。
// 记录不存在返回 ErrNameNotFound。
func (s *Store) Untag(name, tag string) error {
	if err := validateName(name); err != nil {
		return err
	}
	tag = normalizeTag(tag)
	if err := validateTag(tag); err != nil {
		return err
	}

	lk, err := acquireFileLock(s.lockPath(), true)
	if err != nil {
		return err
	}
	defer lk.release()

	refs, err := s.loadRefsLocked()
	if err != nil {
		return err
	}
	tags, ok := refs[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNameNotFound, name)
	}
	if _, ok := tags[tag]; !ok {
		return fmt.Errorf("%w: %s:%s", ErrNameNotFound, name, tag)
	}
	delete(tags, tag)
	if len(tags) == 0 {
		delete(refs, name)
	}
	return s.saveRefsLocked(refs)
}

// ListTags 列出当前全部登记记录，按名字、标签排序。
func (s *Store) ListTags() ([]TagRecord, error) {
	lk, err := acquireFileLock(s.lockPath(), false)
	if err != nil {
		return nil, err
	}
	defer lk.release()

	refs, err := s.loadRefsLocked()
	if err != nil {
		return nil, err
	}
	var out []TagRecord
	for name, tags := range refs {
		for tag, d := range tags {
			out = append(out, TagRecord{Name: name, Tag: tag, Digest: d})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Tag < out[j].Tag
	})
	return out, nil
}

// allTagsSnapshot 返回当前登记映射的快照（内部使用，供 GC 取根）。
func (s *Store) allTagsSnapshot() (map[string]map[string]Digest, error) {
	lk, err := acquireFileLock(s.lockPath(), false)
	if err != nil {
		return nil, err
	}
	defer lk.release()
	return s.loadRefsLocked()
}
